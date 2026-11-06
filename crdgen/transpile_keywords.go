package crdgen

// 2019-09 / 2020-12 keyword handling for the direct transpiler: const, examples, the
// x-kubernetes-* structural extensions, and the conditional/dependency keywords expressed as CEL
// (x-kubernetes-validations) where tractable — else degraded to open objects with a warning.
// Generated CEL is validated by the apiserver's own CRD validation gate (validateCRD), so a bad
// rule fails generation loudly rather than shipping.

import (
	"encoding/json"
	"fmt"
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
func (t *transpiler) copyConst(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
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

// emitNot maps a `not` whose subschema is a scalar const or enum to a native `not: {enum: [...]}`.
func (t *transpiler) emitNot(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	if node.Not == nil {
		return
	}
	if out.Type == "" || out.Type == schemas.TypeNameObject || out.Type == schemas.TypeNameArray {
		t.warn(path, KindConstraintDropped, "not on non-scalar node unsupported -> dropped")
		return
	}
	n := node.Not
	if !isScalarOnly(n) {
		t.warn(path, KindConstraintDropped, "not (non-trivial subschema) unsupported -> dropped")
		return
	}
	// NATIVE `not`, NOT CEL. A structural schema accepts `not` as a pure value validation, so a
	// forbidden const or enum is expressed exactly as authored, enforced by the apiserver with no CEL
	// cost budget. This used to emit `!(self in [...])` as a CEL rule, and the cost estimator
	// multiplies a rule by every enclosing unbounded array and string. A forbidden-values constraint
	// on the items of two unbounded arrays was refused at 1.468006x over budget, and with it the WHOLE
	// CRD: snowplow 1.12.45's chart, then installer 0.3.508's identity-lifecycle schema (a security
	// constraint: RBAC resources must not be "*" or "secrets"). The constraint was never expensive;
	// only its CEL translation was.
	values := n.Enum
	if n.Const != nil {
		values = []any{n.Const}
	}
	if len(values) == 0 {
		t.warn(path, KindConstraintDropped, "not (non-trivial) unsupported -> dropped")
		return
	}
	forbidden := make([]apiextensionsv1.JSON, 0, len(values))
	for _, v := range values {
		raw, err := json.Marshal(v)
		if err != nil {
			t.warn(path, KindConstraintDropped, "not: unmarshalable value -> dropped")
			return
		}
		forbidden = append(forbidden, apiextensionsv1.JSON{Raw: raw})
	}
	out.Not = &apiextensionsv1.JSONSchemaProps{Enum: forbidden}
}

// emitIfThenElse maps a tractable if/then/else (conditions over property const/enum/required) to CEL.
func (t *transpiler) emitIfThenElse(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	if node.If == nil {
		return
	}
	if out.Type != schemas.TypeNameObject {
		t.warn(path, KindConstraintDropped, "if/then/else on non-object -> dropped")
		return
	}
	cond, ok := t.celCond(out, node.If)
	if !ok {
		t.warn(path, KindConstraintDropped, "if condition not expressible in CEL -> if/then/else dropped")
		return
	}
	if node.Then != nil {
		if cons, ok := t.celCond(out, node.Then); ok {
			t.addRule(out, fmt.Sprintf("!(%s) || (%s)", cond, cons), "conditional constraint (then)")
		} else {
			t.warn(path, KindConstraintDropped, "then not expressible in CEL -> dropped")
		}
	}
	if node.Else != nil {
		if cons, ok := t.celCond(out, node.Else); ok {
			t.addRule(out, fmt.Sprintf("(%s) || (%s)", cond, cons), "conditional constraint (else)")
		} else {
			t.warn(path, KindConstraintDropped, "else not expressible in CEL -> dropped")
		}
	}
}

// celCond turns a simple object schema (required + properties constrained by const/enum + a
// whole-self const) into a boolean CEL expression over `self`. Every referenced field must be a
// usable field on `out` (declared property, or the object allows unknowns) so the emitted CEL
// actually compiles. Returns ok=false for anything more complex.
func (t *transpiler) celCond(out *apiextensionsv1.JSONSchemaProps, s *schemas.Type) (string, bool) {
	if s == nil {
		return "", false
	}
	if len(s.Type) > 0 && !s.Type.Equals(schemas.TypeList{schemas.TypeNameObject}) {
		return "", false
	}
	if s.Items != nil || len(s.AllOf) > 0 || len(s.AnyOf) > 0 || len(s.OneOf) > 0 ||
		s.Ref != "" || s.AdditionalProperties != nil || s.Not != nil || s.If != nil {
		return "", false
	}
	var conj []string
	if s.Const != nil {
		lit, ok := celLiteral(s.Const)
		if !ok {
			return "", false
		}
		conj = append(conj, "self == "+lit)
	}
	for _, r := range sortedStrings(s.Required) {
		h, ok := celHas(r)
		if !ok || !fieldAllowed(out, r) {
			return "", false
		}
		conj = append(conj, h)
	}
	for _, k := range sortedPropKeys(s.Properties) {
		p := s.Properties[k]
		has, ok := celHas(k)
		if !ok || !fieldAllowed(out, k) {
			return "", false
		}
		sel, _ := celSelf(k)
		switch {
		case p.Const != nil:
			lit, ok := celLiteral(p.Const)
			if !ok {
				return "", false
			}
			conj = append(conj, fmt.Sprintf("%s && %s == %s", has, sel, lit))
		case len(p.Enum) > 0:
			lits, ok := celLiteralList(p.Enum)
			if !ok {
				return "", false
			}
			conj = append(conj, fmt.Sprintf("%s && (%s in [%s])", has, sel, strings.Join(lits, ", ")))
		default:
			conj = append(conj, has)
		}
	}
	if len(conj) == 0 {
		return "", false
	}
	return strings.Join(conj, " && "), true
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
		t.warn(path, KindConstraintDropped, "prefixItems (tuple positional typing) not representable -> items accepted as-is")
		if out.Type == schemas.TypeNameArray && out.Items == nil {
			out.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: openObject()}
		}
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
