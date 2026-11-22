package crdgen

// 2019-09 / 2020-12 keyword handling for the direct transpiler: const, examples, the
// x-kubernetes-* structural extensions, and the conditional/dependency keywords expressed as CEL
// (x-kubernetes-validations) where tractable — else degraded to open objects with a warning.
// Generated CEL is validated by the apiserver's own CRD validation gate (validateCRD), so a bad
// rule fails generation loudly rather than shipping.

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/krateo-platformops/plumbing/crdgen/schemas"
)

var celIdent = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// inferType picks a structural type when none is declared but a value keyword implies one.
func inferType(node *schemas.Type) string {
	switch {
	case node.Const != nil:
		return jsonKindType(node.Const)
	case len(node.Enum) > 0:
		return jsonKindType(node.Enum[0])
	case len(node.Properties) > 0 || node.AdditionalProperties != nil || len(node.PatternProperties) > 0:
		return schemas.TypeNameObject
	case node.Items != nil || len(node.PrefixItems) > 0:
		return schemas.TypeNameArray
	case len(node.DependentRequired) > 0 || node.If != nil || len(node.DependentSchemas) > 0 || node.UnevaluatedProperties != nil:
		return schemas.TypeNameObject
	}
	return ""
}

func jsonKindType(v any) string {
	switch v.(type) {
	case string:
		return schemas.TypeNameString
	case bool:
		return schemas.TypeNameBoolean
	case float64, json.Number, int, int64:
		return schemas.TypeNameNumber
	case map[string]interface{}:
		return schemas.TypeNameObject
	case []interface{}:
		return schemas.TypeNameArray
	}
	return ""
}

// copyConst maps `const` to a single-value `enum` (native + faithful).
//
// `const: null` ("only null", plumbing#227) is enforced as `enum: [null]` on a typed field that is
// not nullable. Measured on apiserver 1.36, for every type: absent and null are admitted (null is
// not stored on a non-nullable field) and every value is refused, which is the schema's own
// acceptance. On a nullable field the apiserver refuses null itself against `enum: [null]`, so it
// can't be carried: dropped, and said so. An untyped field never gets here (untyped()).
func (t *transpiler) copyConst(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	if node.ConstNull {
		if out.Nullable {
			t.warn(path, KindConstraintDropped, "const: null on a nullable field: the apiserver refuses null against enum [null] -> dropped")
			return
		}
		out.Enum = []apiextensionsv1.JSON{{Raw: []byte("null")}}
		return
	}
	if node.Const == nil {
		return
	}
	raw, err := json.Marshal(node.Const)
	if err != nil {
		t.warn(path, KindConstraintDropped, "unmarshalable const dropped")
		return
	}
	out.Enum = []apiextensionsv1.JSON{{Raw: raw}}
}

// copyExample maps JSON Schema `examples` (an array) to the CRD's single `example`.
func (t *transpiler) copyExample(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	if node.Examples == nil {
		return
	}
	v := node.Examples
	if arr, ok := v.([]interface{}); ok {
		if len(arr) == 0 {
			return
		}
		v = arr[0]
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	out.Example = &apiextensionsv1.JSON{Raw: raw}
}

// passthroughExtensions carries the x-kubernetes-* structural extensions from the input schema
// (int-or-string is handled earlier; embedded-resource/list-type/list-map-keys/map-type and any
// pre-existing x-kubernetes-validations flow through here).
func (t *transpiler) passthroughExtensions(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type) {
	// Objects only, and only true. The apiserver takes true or undefined here, and the marker has
	// always been ignored on a scalar, so a chart carrying it there must keep generating.
	if isPreserved(node) && out.Type == "object" {
		out.XPreserveUnknownFields = boolPtr(true)
	}
	if node.XEmbeddedResource {
		out.XEmbeddedResource = true
	}
	if node.XListType != nil {
		out.XListType = node.XListType
	}
	if len(node.XListMapKeys) > 0 {
		out.XListMapKeys = node.XListMapKeys
	}
	if node.XMapType != nil {
		out.XMapType = node.XMapType
	}
	for _, r := range node.XValidations {
		out.XValidations = append(out.XValidations, toValidationRule(r))
	}
}

// isPreserved reports whether the author set x-kubernetes-preserve-unknown-fields: true.
func isPreserved(node *schemas.Type) bool {
	return node.PreserveUnknownFields != nil && *node.PreserveUnknownFields
}

func toValidationRule(r schemas.ValidationRule) apiextensionsv1.ValidationRule {
	vr := apiextensionsv1.ValidationRule{
		Rule:              r.Rule,
		Message:           r.Message,
		MessageExpression: r.MessageExpression,
		FieldPath:         r.FieldPath,
		OptionalOldSelf:   r.OptionalOldSelf,
	}
	if r.Reason != nil {
		reason := apiextensionsv1.FieldValueErrorReason(*r.Reason)
		vr.Reason = &reason
	}
	return vr
}

func (t *transpiler) addRule(out *apiextensionsv1.JSONSchemaProps, rule, message string) {
	out.XValidations = append(out.XValidations, apiextensionsv1.ValidationRule{Rule: rule, Message: message})
}

// emitDependentRequired maps `dependentRequired` to CEL: {p: [a,b]} -> "!has(self.p) || (has(self.a) && has(self.b))".
func (t *transpiler) emitDependentRequired(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	if len(node.DependentRequired) == 0 {
		return
	}
	if out.Type != schemas.TypeNameObject {
		t.warn(path, KindConstraintDropped, "dependentRequired on non-object -> dropped")
		return
	}
	for _, p := range sortedDepKeys(node.DependentRequired) {
		deps := node.DependentRequired[p]
		if len(deps) == 0 {
			continue
		}
		trigger, ok := celHas(p)
		if !ok || !fieldAllowed(out, p) {
			t.warn(path, KindConstraintDropped, "dependentRequired key not a usable CEL field -> dropped: "+p)
			continue
		}
		conds := make([]string, 0, len(deps))
		ok = true
		for _, d := range deps {
			h, hok := celHas(d)
			if !hok || !fieldAllowed(out, d) {
				ok = false
				break
			}
			conds = append(conds, h)
		}
		if !ok {
			t.warn(path, KindConstraintDropped, "dependentRequired dependency not a usable CEL field -> dropped for "+p)
			continue
		}
		t.addRule(out,
			fmt.Sprintf("!%s || (%s)", trigger, strings.Join(conds, " && ")),
			fmt.Sprintf("when %s is set, these must also be set: %s", p, strings.Join(deps, ", ")))
	}
}

// emitNot maps a `not` whose subschema is scalar-only to a NATIVE `not`, carrying the subschema's
// value validations as authored: forbidden values (const/enum), pattern, format, length, numeric
// bounds and multipleOf (plumbing#102).
func (t *transpiler) emitNot(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	if node.Not == nil {
		return
	}
	if out.Type == "" || out.Type == schemas.TypeNameArray {
		t.warn(path, KindConstraintDropped, "not on non-scalar node unsupported -> dropped")
		return
	}
	n := node.Not
	if !isScalarOnly(n) && !isScalarJunction(n) {
		t.warn(path, KindConstraintDropped, "not (non-trivial subschema) unsupported -> dropped")
		return
	}
	// NATIVE `not`, NOT CEL. A structural schema accepts any VALUE VALIDATION inside a junctor, `not`
	// included (apiextensions validateNestedValueValidation), so the constraint is expressed exactly as
	// authored and enforced by the apiserver with no CEL cost budget. This used to emit
	// `!(self in [...])` as a CEL rule, and the cost estimator multiplies a rule by every enclosing
	// unbounded array and string. A forbidden-values constraint on the items of two unbounded arrays
	// was refused at 1.468006x over budget, and with it the WHOLE CRD: snowplow 1.12.45's chart, then
	// installer 0.3.508's identity-lifecycle schema (a security constraint: RBAC resources must not be
	// "*" or "secrets"). The constraint was never expensive; only its CEL translation was.
	//
	// plumbing#102: only const/enum were carried, and every other scalar `not` was DROPPED, e.g.
	// agentgateway-policies 0.2.20's `not: {pattern: ...}` on four sampling fields, unenforced on 057.
	// The subschema is built from value validations only (junctorMemberSchema, the same mapping the
	// main path uses for scalars, plus format), so it holds nothing a junctor forbids and needs no
	// sanitizing: a mutant that removed a sanitizeJunctorMember call here changed no output.
	//
	// plumbing#48: a `not` over allOf/anyOf/oneOf of such value validations is native too, e.g.
	// not: {anyOf: [{enum: ["*"]}, {pattern: "^system:"}]}, "refuse * and every system: group", which
	// was dropped and left the CRD admitting exactly what the author forbade.
	//
	// And on an OBJECT, for the object's own value validations: `required` and the property-count
	// bounds. core-provider's chart forbids two keys in an env map with
	// not: {anyOf: [{required: [GOMEMLIMIT]}, {required: [COMPOSITION_CONTROLLER_HELM_TIMEOUT]}]},
	// which was the last `not` crdgen dropped across 1,187 local values schemas. A structural schema
	// accepts `required` and min/maxProperties inside a junctor, so it is carried exactly as written,
	// with no CEL. Anything else object-shaped (properties, patternProperties, dependentRequired)
	// still drops the whole `not`, because a part dropped inside a `not` makes it refuse MORE.
	sub, reason := notValueSchema(n, out.Type)
	if reason == "" {
		for _, j := range []struct {
			members []*schemas.Type
			dst     *[]apiextensionsv1.JSONSchemaProps
		}{{n.AllOf, &sub.AllOf}, {n.AnyOf, &sub.AnyOf}, {n.OneOf, &sub.OneOf}} {
			for _, m := range j.members {
				ms, r := notValueSchema(m, out.Type)
				if r == "" && isVacuousJunctorMember(*ms) {
					r = "a junction member carries nothing the CRD can express"
				}
				if r != "" {
					reason = r
					break
				}
				*j.dst = append(*j.dst, *ms)
			}
			if reason != "" {
				break
			}
		}
	}
	if reason != "" {
		t.warn(path, KindConstraintDropped, "not: "+reason+" -> dropped")
		return
	}
	// A subschema that carries nothing the CRD can express (e.g. only minItems on a string, which
	// constrains no string) would become `not: {}`, which matches everything and so
	// REJECTS every value. Drop it instead, loudly.
	if isVacuousJunctorMember(*sub) {
		t.warn(path, KindConstraintDropped, "not (non-trivial) unsupported -> dropped")
		return
	}
	out.Not = sub
}

// isScalarJunction reports whether n is value validations plus allOf/anyOf/oneOf whose members are
// value validations only: one level, nothing structural anywhere (plumbing#48).
func isScalarJunction(n *schemas.Type) bool {
	if len(n.Properties) != 0 || n.Items != nil || n.Ref != "" || n.AdditionalProperties != nil || n.If != nil || n.Not != nil {
		return false
	}
	if len(n.AllOf)+len(n.AnyOf)+len(n.OneOf) == 0 {
		return false
	}
	for _, js := range [][]*schemas.Type{n.AllOf, n.AnyOf, n.OneOf} {
		for _, m := range js {
			if m == nil || !isScalarOnly(m) {
				return false
			}
		}
	}
	return true
}

// notValueSchema builds the value validations of one schema inside a `not` for a field of nodeType,
// or says why it cannot be built FAITHFULLY. Inside a `not` every loss inverts: a part dropped from
// the subschema makes it match more, so the `not` refuses MORE than the author wrote, and the CRD
// rejects valid objects. So a part that cannot be carried drops the whole `not` (reported) instead:
//
//   - a type other than the field's own: such a member never matches the field's values, and with
//     its type dropped it would match them (not: {type: integer, minLength: 2} on a string refuses
//     nothing; without the type it refused every string of length 2 or more). An absent type, or
//     the field's own type, is redundant and dropped losslessly;
//   - a bound copyScalarValidation could not express (plumbing#110);
//   - an enum or const value that cannot be serialised.
//
// Keywords that do not apply to the field's scalar values (array or object sizes) constrain
// nothing there, so leaving them out loses nothing. On an object, min/maxProperties DO apply, and are
// carried; `required` is carried on every node, as it always was.
func notValueSchema(m *schemas.Type, nodeType string) (*apiextensionsv1.JSONSchemaProps, string) {
	if m.ConstNull {
		return nil, "const: null" // plumbing#227: without it the subschema matches more, so the not refuses more
	}
	if nodeType == schemas.TypeNameObject && (len(m.PatternProperties) > 0 || len(m.DependentRequired) > 0 || len(m.DependentSchemas) > 0) {
		return nil, "an object keyword the CRD cannot carry inside a not"
	}
	if len(m.Type) > 0 {
		pt, _ := primaryType(m.Type)
		if len(m.Type) > 1 || pt != nodeType {
			return nil, fmt.Sprintf("a type %v other than the field's %s", []string(m.Type), nodeType)
		}
	}
	sub, dropped := junctorMemberSchema(m)
	if len(dropped) > 0 {
		return nil, dropped[0] + " (so the not cannot be carried whole)"
	}
	sub.Description = ""
	sub.Format = m.Format
	if nodeType == schemas.TypeNameObject {
		// The object's own size bounds apply to it, so they are carried; on a scalar they constrain
		// nothing and are left out losslessly (below).
		if m.MinProperties > 0 {
			v := int64(m.MinProperties)
			sub.MinProperties = &v
		}
		if m.MaxProperties > 0 {
			v := int64(m.MaxProperties)
			sub.MaxProperties = &v
		}
	}
	values := m.Enum
	if m.Const != nil {
		values = []any{m.Const}
	}
	for _, v := range values {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, "unmarshalable value"
		}
		sub.Enum = append(sub.Enum, apiextensionsv1.JSON{Raw: raw})
	}
	return sub, ""
}

// emitIfThenElse maps a tractable if/then/else to CEL: conditions and branches over required and
// over property constraints celProperty can express (plumbing#211).
//
// The condition must translate completely: dropping a conjunct of `if` would widen it and apply
// then/else where the schema doesn't, refusing objects the schema accepts. A branch may translate
// in part: a property constraint it can't express is dropped and reported, and the rest of the
// branch is enforced.
func (t *transpiler) emitIfThenElse(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	if node.If == nil {
		return
	}
	if out.Type != schemas.TypeNameObject {
		t.warn(path, KindConstraintDropped, "if/then/else on non-object -> dropped")
		return
	}
	cond, dropped, ok := t.celCond(out, node.If, true)
	if !ok || len(dropped) > 0 {
		t.warn(path, KindConstraintDropped, "if condition not expressible in CEL -> if/then/else dropped")
		return
	}
	if cond == "" {
		cond = "true" // a condition that checks nothing holds for every object
	}
	branch := func(b *schemas.Type, name, format string) {
		if b == nil {
			return
		}
		cons, dropped, ok := t.celCond(out, b, false)
		if !ok {
			t.warn(path, KindConstraintDropped, name+" not expressible in CEL -> dropped")
			return
		}
		for _, d := range dropped {
			t.warn(path, KindConstraintDropped, fmt.Sprintf("%s: %s -> dropped", name, d))
		}
		if cons != "" {
			t.addRule(out, fmt.Sprintf(format, cond, cons), "conditional constraint ("+name+")")
		}
	}
	branch(node.Then, "then", "!(%s) || (%s)")
	branch(node.Else, "else", "(%s) || (%s)")
}

// celCond turns a simple object schema (required, properties whose constraints celProperty
// expresses, a whole-self const) into a boolean CEL expression over `self`. Every referenced field
// must be a usable field on `out` (declared property, or the object allows unknowns) so the emitted
// CEL compiles.
//
// A property clause holds where the property is absent, as JSON Schema's `properties` does: it is
// `!has(self.k) || P`, or `has(self.k) && P` where k is always present, so the two agree: the node
// or the schema itself requires k, or, in a condition (isIf), the CRD defaults k. The apiserver
// defaults a property before it validates an object, so a defaulted one is never absent there; the
// one place it is absent is the apiserver's check of a default literal against the schema (a
// parent's materialized `{}`), where a condition matching on its absence would refuse the CRD's
// own default. A clause with nothing to check adds nothing; `required` adds the has().
//
// dropped names each property clause celProperty can't express, which the expression leaves out.
// An empty expression with nothing dropped is a schema that checks nothing (every object satisfies
// it). ok=false is anything else it can't express.
func (t *transpiler) celCond(out *apiextensionsv1.JSONSchemaProps, s *schemas.Type, isIf bool) (expr string, dropped []string, ok bool) {
	if s == nil {
		return "", nil, false
	}
	if len(s.Type) > 0 && !s.Type.Equals(schemas.TypeList{schemas.TypeNameObject}) {
		return "", nil, false
	}
	if s.Items != nil || len(s.AllOf) > 0 || len(s.AnyOf) > 0 || len(s.OneOf) > 0 ||
		s.Ref != "" || s.AdditionalProperties != nil || s.Not != nil || s.If != nil {
		return "", nil, false
	}
	known := map[string]bool{}
	for _, r := range out.Required {
		known[r] = true
	}
	if isIf {
		for k, p := range out.Properties {
			if p.Default != nil {
				known[k] = true
			}
		}
	}
	var conj []string
	if s.ConstNull {
		return "", nil, false // plumbing#227: "only null" for the object itself isn't expressed
	}
	if s.Const != nil {
		lit, ok := celLiteral(s.Const)
		if !ok {
			return "", nil, false
		}
		conj = append(conj, "self == "+lit)
	}
	for _, r := range sortedStrings(s.Required) {
		h, ok := celHas(r)
		if !ok || !fieldAllowed(out, r) {
			return "", nil, false
		}
		conj = append(conj, h)
		known[r] = true
	}
	for _, k := range sortedPropKeys(s.Properties) {
		has, ok := celHas(k)
		if !ok || !fieldAllowed(out, k) {
			return "", nil, false
		}
		sel, _ := celSelf(k)
		preds, why := celProperty(out.Properties[k], sel, s.Properties[k])
		switch {
		case why != "":
			dropped = append(dropped, fmt.Sprintf("property %q: %s", k, why))
		case len(preds) == 0:
		case known[k]:
			conj = append(conj, has+" && "+strings.Join(preds, " && "))
		default:
			conj = append(conj, "(!"+has+" || "+strings.Join(preds, " && ")+")")
		}
	}
	return strings.Join(conj, " && "), dropped, true
}

// celPropertyKeywords are the keywords a property clause of a condition may carry: the ones
// celProperty translates, plus annotations, which constrain nothing.
var celPropertyKeywords = map[string]bool{
	"type": true, "const": true, "enum": true,
	"minLength": true, "maxLength": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
	"minItems": true, "maxItems": true, "minProperties": true, "maxProperties": true,
	"title": true, "description": true, "default": true, "examples": true, "readOnly": true,
}

// celProperty translates one property clause of a condition, p, into predicates over sel, the
// property's CEL selector; decl is the property as the CRD declares it. A keyword applies only to
// the kind of value it constrains, as in JSON Schema, so a length bound on an integer property
// checks nothing. why says what it can't express, and then preds is nil.
func celProperty(decl apiextensionsv1.JSONSchemaProps, sel string, p *schemas.Type) (preds []string, why string) {
	if p == nil {
		return nil, ""
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, "not marshalable"
	}
	var kw map[string]json.RawMessage
	if err := json.Unmarshal(raw, &kw); err != nil {
		return nil, "not marshalable"
	}
	var unsupported []string
	for k := range kw {
		if !celPropertyKeywords[k] {
			unsupported = append(unsupported, k)
		}
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, strings.Join(unsupported, ", ") + " not expressible in CEL"
	}
	if len(p.Type) > 0 && !(len(p.Type) == 1 && p.Type[0] == decl.Type) {
		return nil, fmt.Sprintf("type %v differs from the declared %q", []string(p.Type), decl.Type)
	}
	if p.ConstNull {
		return nil, "const: null not expressible in CEL" // plumbing#227: dropping it would widen the clause
	}
	if p.Const != nil {
		lit, ok := celLiteral(p.Const)
		if !ok {
			return nil, "const not expressible in CEL"
		}
		preds = append(preds, sel+" == "+lit)
	}
	if len(p.Enum) > 0 {
		lits, ok := celLiteralList(p.Enum)
		if !ok {
			return nil, "enum not expressible in CEL"
		}
		preds = append(preds, fmt.Sprintf("(%s in [%s])", sel, strings.Join(lits, ", ")))
	}

	sizes := func(min, max int) {
		if min > 0 {
			preds = append(preds, fmt.Sprintf("size(%s) >= %d", sel, min))
		}
		if max > 0 {
			preds = append(preds, fmt.Sprintf("size(%s) <= %d", sel, max))
		}
	}
	numeric := p.Minimum != nil || p.Maximum != nil || p.ExclusiveMinimum != nil || p.ExclusiveMaximum != nil
	switch {
	case decl.XIntOrString || decl.Type == "":
		if numeric || p.MinLength > 0 || p.MaxLength > 0 || p.MinItems > 0 || p.MaxItems > 0 || p.MinProperties > 0 || p.MaxProperties > 0 {
			return nil, "a bound on a property with no single declared type"
		}
	case decl.Type == schemas.TypeNameString:
		sizes(p.MinLength, p.MaxLength)
	case decl.Type == schemas.TypeNameArray:
		sizes(p.MinItems, p.MaxItems)
	case decl.Type == schemas.TypeNameObject:
		if p.MinProperties > 0 || p.MaxProperties > 0 {
			// CEL's size() is defined on a map, not on an object with declared properties.
			if decl.AdditionalProperties == nil || len(decl.Properties) > 0 {
				return nil, "minProperties/maxProperties on an object that isn't a map"
			}
			sizes(p.MinProperties, p.MaxProperties)
		}
	case decl.Type == schemas.TypeNameInteger || decl.Type == schemas.TypeNameNumber:
		bounds, why := celNumericBounds(sel, p, decl.Type == schemas.TypeNameInteger)
		if why != "" {
			return nil, why
		}
		preds = append(preds, bounds...)
	}
	return preds, ""
}

// celNumericBounds renders minimum, maximum and the exclusive bounds (a number, or draft 4's
// boolean that makes minimum/maximum strict) as comparisons. On an integer property a fractional
// bound is rounded to the integer comparison it is equal to, so the literal is an int.
func celNumericBounds(sel string, p *schemas.Type, integer bool) ([]string, string) {
	type bound struct {
		op string
		v  float64
	}
	var bs []bound
	strict := func(e *any, op, strictOp string, base *float64) (string, bool) {
		if e == nil {
			if base != nil {
				bs = append(bs, bound{op, *base})
			}
			return "", true
		}
		switch x := (*e).(type) {
		case bool:
			if base == nil {
				return "", true
			}
			if x {
				bs = append(bs, bound{strictOp, *base})
			} else {
				bs = append(bs, bound{op, *base})
			}
		case float64:
			bs = append(bs, bound{strictOp, x})
			if base != nil {
				bs = append(bs, bound{op, *base})
			}
		default:
			return fmt.Sprintf("exclusive bound %v is neither a number nor a boolean", x), false
		}
		return "", true
	}
	if why, ok := strict(p.ExclusiveMinimum, ">=", ">", p.Minimum); !ok {
		return nil, why
	}
	if why, ok := strict(p.ExclusiveMaximum, "<=", "<", p.Maximum); !ok {
		return nil, why
	}
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		if math.IsNaN(b.v) || math.IsInf(b.v, 0) {
			return nil, "a bound that isn't a finite number"
		}
		if !integer {
			// CEL compares a double with an int literal, so a whole bound needs no ".0".
			out = append(out, fmt.Sprintf("%s %s %s", sel, b.op, strconv.FormatFloat(b.v, 'g', -1, 64)))
			continue
		}
		v := b.v
		switch b.op {
		case ">=", "<":
			v = math.Ceil(v)
		case ">", "<=":
			v = math.Floor(v)
		}
		if math.Abs(v) > 1<<53 {
			return nil, "an integer bound beyond 2^53"
		}
		out = append(out, fmt.Sprintf("%s %s %d", sel, b.op, int64(v)))
	}
	return out, ""
}

// handleUnsupported degrades keywords a structural CRD cannot express, always with a warning.
func (t *transpiler) handleUnsupported(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	widen := func() {
		if out.Type == schemas.TypeNameObject && out.AdditionalProperties == nil {
			out.XPreserveUnknownFields = boolPtr(true)
		}
	}
	if len(node.PatternProperties) > 0 {
		t.warn(path, KindStructureLoosened, "patternProperties unsupported -> preserve-unknown")
		widen()
	}
	if len(node.DependentSchemas) > 0 {
		t.warn(path, KindConstraintDropped, "dependentSchemas unsupported -> dropped")
	}
	if len(node.PrefixItems) > 0 {
		// The array case of transpile has already set Items (the declared items, or anyValue when
		// there are none), so "accepted as-is" holds without a fallback here (plumbing#186).
		t.warn(path, KindConstraintDropped, "prefixItems (tuple positional typing) not representable -> items accepted as-is")
	}
	if node.UnevaluatedProperties != nil {
		closedFalse := node.UnevaluatedProperties.IsBool && !node.UnevaluatedProperties.Bool
		if !closedFalse {
			t.warn(path, KindStructureLoosened, "unevaluatedProperties (schema/true) -> preserve-unknown")
			widen()
		}
	}
	if node.UnevaluatedItems != nil {
		t.warn(path, KindConstraintDropped, "unevaluatedItems unsupported -> ignored")
	}
}

func isScalarOnly(n *schemas.Type) bool {
	return len(n.Properties) == 0 && n.Items == nil && len(n.AllOf) == 0 && len(n.AnyOf) == 0 &&
		len(n.OneOf) == 0 && n.Ref == "" && n.AdditionalProperties == nil && n.If == nil && n.Not == nil
}

// fieldAllowed reports whether a CEL `has(self.<name>)` on this node will compile: the field is a
// declared property, or the object accepts unknown fields (preserve-unknown / additionalProperties).
func fieldAllowed(out *apiextensionsv1.JSONSchemaProps, name string) bool {
	if out.XPreserveUnknownFields != nil && *out.XPreserveUnknownFields {
		return true
	}
	if out.AdditionalProperties != nil {
		return true
	}
	_, ok := out.Properties[name]
	return ok
}

func celHas(field string) (string, bool) {
	if !celIdent.MatchString(field) {
		return "", false
	}
	return "has(self." + field + ")", true
}

func celSelf(field string) (string, bool) {
	if !celIdent.MatchString(field) {
		return "", false
	}
	return "self." + field, true
}

// celLiteral renders a scalar JSON value as a CEL literal.
func celLiteral(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		b, _ := json.Marshal(x)
		return string(b), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), true
	case json.Number:
		return x.String(), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case nil:
		return "null", true
	default:
		return "", false
	}
}

func celLiteralList(vals []interface{}) ([]string, bool) {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		l, ok := celLiteral(v)
		if !ok {
			return nil, false
		}
		out = append(out, l)
	}
	return out, true
}

func sortedDepKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPropKeys(m map[string]*schemas.Type) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}
