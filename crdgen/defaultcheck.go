package crdgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/krateo-platformops/plumbing/crdgen/schemas"
)

// DefaultContradictionError reports `default`s that crdgen would put into the CRD although they do
// not satisfy the subschema they are written in (plumbing#36). It is a CONFIG error: the input
// schema contradicts itself, and no change to crdgen can fix it. Match it with errors.As.
//
// # Why crdgen checks this itself
//
// JSON Schema treats `default` as an annotation: it is never validated, so helm accepts a schema
// such as
//
//	"security": {"type": "object", "default": {}, "required": ["authentication"],
//	             "properties": {"authentication": {"type": "object"}}}
//
// A Kubernetes structural schema does validate a default, literally, against the schema it sits in.
// crdgen copies the default into the CRD, so the apiserver's own gate refuses the CRD with
//
//	...properties[security].default.authentication: Required value
//
// and crdgen used to wrap that as "this is a crdgen bug, not a config error". The blame was wrong,
// and it sent people to read crdgen when the chart was the only thing that could be fixed. Checking
// the INPUT, against the input's own subschema, is what makes the blame exact.
//
// # Only defaults that reach the CRD fail generation
//
// crdgen does not carry every default: one inside an anyOf/oneOf member or an allOf, under `not` or
// if/then/else, in an unreferenced $defs entry, or on a node with no usable type never reaches the
// CRD, so the apiserver never saw it and the CRD installed. Failing generation for one of those
// would break a CRD that works today. Those are reported as KindDefaultContradictsSchema warnings
// instead, with the same pointer and reasons (decided by Diego, 2026-11-16, on oasgen-provider's
// review of #205).
type DefaultContradictionError struct {
	// Schema names the input: "spec schema" (Options.SpecSchema, a chart's values.schema.json) or
	// "status schema" (Options.StatusSchema).
	Schema string
	// Contradictions is sorted by Pointer.
	Contradictions []DefaultContradiction
}

// DefaultContradiction is one `default` that fails its own subschema.
type DefaultContradiction struct {
	// Pointer is the JSON pointer, in the input document, of the subschema holding the default,
	// spelled as the document spells it (e.g. "#/properties/security", "#/definitions/Tls").
	Pointer string
	// Default is the default as written.
	Default json.RawMessage
	// Reasons are the validator's failures, each prefixed with the location inside the default
	// value it concerns ("at '': missing property 'authentication'").
	Reasons []string
}

func (c DefaultContradiction) describe() string {
	return fmt.Sprintf("default %s: %s", c.Default, strings.Join(c.Reasons, ", "))
}

func (e *DefaultContradictionError) Error() string {
	var sb strings.Builder
	noun := "default does"
	if len(e.Contradictions) != 1 {
		noun = "defaults do"
	}
	fmt.Fprintf(&sb, "%s: %d %s not satisfy the schema it is written in, and a CRD default must "+
		"(this is a config error in the input schema, not a crdgen bug): ", e.Schema, len(e.Contradictions), noun)
	for i, c := range e.Contradictions {
		if i > 0 {
			sb.WriteString("; ")
		}
		fmt.Fprintf(&sb, "%s: %s", c.Pointer, c.describe())
	}
	return sb.String()
}

// defaultCheck is what checkDefaults learned about one input schema, before anyone knows which of
// its defaults reach the CRD.
type defaultCheck struct {
	// schema is "spec schema" or "status schema".
	schema string
	// passed, contradictions and unchecked partition the defaults, keyed by pointer.
	passed         map[string]bool
	contradictions map[string]DefaultContradiction
	// unchecked says why a default could NOT be validated. Such a default is never blamed: the
	// check only blames the input when it has proved the input wrong.
	unchecked map[string]string
	// pointerOf maps a node of crdgen's parsed model to its pointer in the raw document, so a default
	// the transpiler carried can be found among the results above.
	pointerOf map[*schemas.Type]string
}

// schemaDocURL is the in-memory location the input is registered under. Nothing is ever loaded from
// it: refusingLoader below turns every load into an error.
const schemaDocURL = "mem:///values.schema.json"

// refusingLoader refuses every URL. crdgen resolves only references inside the document it is given
// (collectRefs), and the check must not reach further than generation does: jsonschema's default
// loader reads local files, and a $ref to a file or an http URL would turn a CRD generation into a
// filesystem or network read.
type refusingLoader struct{}

func (refusingLoader) Load(u string) (any, error) {
	return nil, fmt.Errorf("crdgen resolves only references inside the schema document, not %q", u)
}

// checkDefaults validates every `default` in the raw input schema against the subschema it is
// written in. parsed is crdgen's own parse of the same bytes, the tree the transpiler walks.
//
// The walk is over the RAW document rather than crdgen's parsed model because the model normalizes
// spelling (`definitions` becomes `$defs`, `dependencies` becomes `dependentSchemas`), and a pointer
// the author cannot find in their own file would be no help.
//
// The draft is the document's own `$schema`, or the validator's latest when there is none, which is
// what helm does with a chart's values.schema.json (chartutil.ValidateAgainstSingleSchema).
func checkDefaults(schemaName string, raw []byte, parsed *schemas.Schema) defaultCheck {
	res := defaultCheck{
		schema:         schemaName,
		passed:         map[string]bool{},
		contradictions: map[string]DefaultContradiction{},
		unchecked:      map[string]string{},
		pointerOf:      map[*schemas.Type]string{},
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		// crdgen parsed these bytes already, so this does not happen; with no document there are no
		// pointers, and every carried default is reported unmapped.
		return res
	}
	if parsed != nil {
		indexParsed(doc, parsed, &res)
	}
	sites := collectDefaultSites(doc, nil)
	if len(sites) == 0 {
		return res
	}

	c := jsonschema.NewCompiler()
	c.UseLoader(refusingLoader{})
	if err := c.AddResource(schemaDocURL, doc); err != nil {
		for _, s := range sites {
			res.unchecked[pointerString(s.tokens)] = fmt.Sprintf("the document is not a schema the validator accepts: %v", firstLine(err.Error()))
		}
		return res
	}

	for _, s := range sites {
		ptr := pointerString(s.tokens)
		sch, err := c.Compile(schemaDocURL + "#" + fragmentEscape(s.tokens))
		if err != nil {
			res.unchecked[ptr] = firstLine(err.Error())
			continue
		}
		verr := sch.Validate(s.value)
		if verr == nil {
			res.passed[ptr] = true
			continue
		}
		var ve *jsonschema.ValidationError
		if !errors.As(verr, &ve) {
			res.unchecked[ptr] = firstLine(verr.Error())
			continue
		}
		rendered, _ := json.Marshal(s.value)
		res.contradictions[ptr] = DefaultContradiction{Pointer: ptr, Default: rendered, Reasons: leafReasons(ve)}
	}
	return res
}

// defaultVerdict is checkDefaults' result restricted by what the transpiler actually carried.
type defaultVerdict struct {
	// err is non-nil when a carried default contradicts its schema.
	err *DefaultContradictionError
	// warnings report contradictions in defaults that do not reach the CRD.
	warnings []Warning
	// carriedPassed counts carried defaults proven to satisfy their schema.
	carriedPassed int
	// carriedUnknown describes carried defaults that could not be checked or not located.
	carriedUnknown []string
}

// verdict splits the check by the set of input nodes whose default survives in the generated CRD.
func (c defaultCheck) verdict(carried map[*schemas.Type]bool) defaultVerdict {
	var v defaultVerdict
	carriedPtr := map[string]bool{}
	unmapped := 0
	for n := range carried {
		ptr, ok := c.pointerOf[n]
		if !ok {
			unmapped++
			continue
		}
		carriedPtr[ptr] = true
	}
	if unmapped > 0 {
		v.carriedUnknown = append(v.carriedUnknown, fmt.Sprintf("%s: %d default(s) carried from a node "+
			"the check could not locate in the document", c.schema, unmapped))
	}

	var errs []DefaultContradiction
	for ptr, dc := range c.contradictions {
		if carriedPtr[ptr] {
			errs = append(errs, dc)
			continue
		}
		v.warnings = append(v.warnings, Warning{Path: ptr, Kind: KindDefaultContradictsSchema, Message: fmt.Sprintf(
			"%s: default %s does not satisfy the schema it is written in (%s); crdgen does not carry this "+
				"default into the CRD, so generation proceeds", c.schema, dc.Default, strings.Join(dc.Reasons, ", "))})
	}
	for ptr := range carriedPtr {
		if c.passed[ptr] {
			v.carriedPassed++
		}
		if why, ok := c.unchecked[ptr]; ok {
			v.carriedUnknown = append(v.carriedUnknown, fmt.Sprintf("%s %s: %s", c.schema, ptr, why))
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Pointer < errs[j].Pointer })
	sort.Slice(v.warnings, func(i, j int) bool { return v.warnings[i].Path < v.warnings[j].Path })
	sort.Strings(v.carriedUnknown)
	if len(errs) > 0 {
		v.err = &DefaultContradictionError{Schema: c.schema, Contradictions: errs}
	}
	return v
}

type defaultSite struct {
	tokens []string
	value  any
}

// Keywords whose value is a subschema, across drafts 4 to 2020-12. Anything else is data (`enum`,
// `const`, `default`, `examples`) or an annotation, and is not descended: a property NAMED
// "default" under `properties` is a subschema, but the value of the `default` keyword is not.
var (
	schemaKeywords = []string{
		"additionalItems", "additionalProperties", "contains", "contentSchema", "else", "if", "items",
		"not", "propertyNames", "then", "unevaluatedItems", "unevaluatedProperties",
	}
	schemaArrayKeywords = []string{"allOf", "anyOf", "items", "oneOf", "prefixItems"}
	schemaMapKeywords   = []string{"$defs", "definitions", "dependentSchemas", "dependencies", "patternProperties", "properties"}
)

// collectDefaultSites returns every subschema in doc that carries a `default`, with its pointer.
func collectDefaultSites(node any, at []string) []defaultSite {
	obj, ok := node.(map[string]any)
	if !ok {
		return nil // a boolean schema, or data
	}
	var out []defaultSite
	if v, ok := obj["default"]; ok {
		out = append(out, defaultSite{tokens: append([]string(nil), at...), value: v})
	}
	child := func(v any, tokens ...string) {
		out = append(out, collectDefaultSites(v, append(append([]string(nil), at...), tokens...))...)
	}
	for _, kw := range schemaKeywords {
		if v, ok := obj[kw]; ok {
			child(v, kw)
		}
	}
	for _, kw := range schemaArrayKeywords {
		if arr, ok := obj[kw].([]any); ok {
			for i, v := range arr {
				child(v, kw, fmt.Sprint(i))
			}
		}
	}
	for _, kw := range schemaMapKeywords {
		m, ok := obj[kw].(map[string]any)
		if !ok {
			continue
		}
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			// `dependencies` mixes subschemas with property-name arrays (draft 4-7); an array is
			// not a schema, and collectDefaultSites ignores anything that is not an object.
			child(m[name], kw, name)
		}
	}
	return out
}

// pointerString renders tokens as a JSON pointer prefixed with "#", escaping "~" and "/" (RFC 6901).
func pointerString(tokens []string) string {
	var sb strings.Builder
	sb.WriteString("#")
	for _, t := range tokens {
		sb.WriteString("/")
		sb.WriteString(escapeToken(t))
	}
	return sb.String()
}

// fragmentEscape renders tokens as a URI fragment: a JSON pointer whose tokens are also
// percent-encoded, so a property name holding a space, "%", "#" or "?" addresses the right node.
func fragmentEscape(tokens []string) string {
	var sb strings.Builder
	for _, t := range tokens {
		sb.WriteString("/")
		sb.WriteString(url.PathEscape(escapeToken(t)))
	}
	return sb.String()
}

func escapeToken(t string) string {
	return strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1")
}

// leafReasons flattens a validation error to its leaf failures, each with the location inside the
// default value it concerns.
func leafReasons(ve *jsonschema.ValidationError) []string {
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			unit := e.BasicOutput()
			msg := ""
			if unit.Error != nil {
				msg = unit.Error.String()
			}
			out = append(out, fmt.Sprintf("at '%s': %s", pointerTail(e.InstanceLocation), msg))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

func pointerTail(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	return strings.TrimPrefix(pointerString(tokens), "#")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// refusalAttribution says what is known about the cause when the apiserver refuses a generated CRD
// none of whose carried defaults contradicts its schema.
//
// It does not claim the refusal is a crdgen bug. The input can still cause one in ways this check
// does not cover: an author's own x-kubernetes-validations rule over its CEL cost budget, or a
// `pattern` that is valid ECMA-262 but not RE2. The apiserver's field paths, which follow, are what
// locate it.
func refusalAttribution(verdicts ...defaultVerdict) string {
	passed := 0
	var unknown []string
	for _, v := range verdicts {
		passed += v.carriedPassed
		unknown = append(unknown, v.carriedUnknown...)
	}
	if len(unknown) > 0 {
		return fmt.Sprintf("%d default(s) carried into the CRD could not be checked against their own schema, "+
			"so a contradictory default is not ruled out: %s", len(unknown), strings.Join(unknown, "; "))
	}
	if passed == 0 {
		return "no default from the input is carried into the CRD, so a contradictory default is not the cause"
	}
	return fmt.Sprintf("all %d default(s) carried into the CRD satisfy their own schema, so a contradictory default is not the cause", passed)
}

// indexParsed walks the raw document and crdgen's parsed model in step, recording each parsed node's
// pointer as the document spells it. A keyword the model folds into another (`definitions` into
// `$defs`, `dependencies` into `dependentSchemas`) maps to the same parsed field, with the model's
// own precedence: the new spelling wins when both are present.
//
// The ROOT is special: its $defs live on the schemas.Schema wrapper, which reads both spellings.
func indexParsed(doc any, parsed *schemas.Schema, res *defaultCheck) {
	if parsed.ObjectAsType == nil {
		return
	}
	root := (*schemas.Type)(parsed.ObjectAsType)
	indexNode(doc, root, nil, res)
	obj, _ := doc.(map[string]any)
	for _, kw := range []string{"$defs", "definitions"} {
		if m, ok := obj[kw].(map[string]any); ok {
			if kw == "definitions" && obj["$defs"] != nil {
				continue
			}
			for name, v := range m {
				indexNode(v, parsed.Definitions[name], []string{kw, name}, res)
			}
		}
	}
}

func indexNode(raw any, node *schemas.Type, at []string, res *defaultCheck) {
	obj, ok := raw.(map[string]any)
	if !ok || node == nil {
		return
	}
	if _, seen := res.pointerOf[node]; !seen {
		res.pointerOf[node] = pointerString(at)
	}
	sub := func(kw string, n *schemas.Type, tokens ...string) {
		indexNode(dig(obj[kw], tokens[1:]...), n, append(append([]string(nil), at...), tokens...), res)
	}
	single := map[string]*schemas.Type{
		"items": node.Items, "additionalItems": node.AdditionalItems, "not": node.Not, "if": node.If,
		"then": node.Then, "else": node.Else, "unevaluatedItems": node.UnevaluatedItems, "contentSchema": node.ContentSchema,
	}
	if node.AdditionalProperties != nil {
		single["additionalProperties"] = node.AdditionalProperties.Type
	}
	if node.UnevaluatedProperties != nil {
		single["unevaluatedProperties"] = node.UnevaluatedProperties.Type
	}
	for kw, n := range single {
		sub(kw, n, kw)
	}
	for kw, list := range map[string][]*schemas.Type{"allOf": node.AllOf, "anyOf": node.AnyOf, "oneOf": node.OneOf, "prefixItems": node.PrefixItems} {
		for i, n := range list {
			sub(kw, n, kw, fmt.Sprint(i))
		}
	}
	maps := []struct {
		kw, shadowedBy string
		m              map[string]*schemas.Type
	}{
		{"properties", "", node.Properties},
		{"patternProperties", "", node.PatternProperties},
		{"$defs", "", node.Definitions},
		{"definitions", "$defs", node.Definitions},
		{"dependentSchemas", "", node.DependentSchemas},
		{"dependencies", "dependentSchemas", node.DependentSchemas},
	}
	for _, e := range maps {
		if e.shadowedBy != "" && obj[e.shadowedBy] != nil {
			continue
		}
		if _, ok := obj[e.kw].(map[string]any); !ok {
			continue
		}
		for name, n := range e.m {
			sub(e.kw, n, e.kw, name)
		}
	}
}

// dig follows tokens (array indices or object keys) into raw.
func dig(raw any, tokens ...string) any {
	for _, t := range tokens {
		switch v := raw.(type) {
		case map[string]any:
			raw = v[t]
		case []any:
			var i int
			if _, err := fmt.Sscan(t, &i); err != nil || i < 0 || i >= len(v) {
				return nil
			}
			raw = v[i]
		default:
			return nil
		}
	}
	return raw
}

// survivingDefaults returns every default value in the generated schema, by identity. copyDefault
// allocates one *JSON per carried default and every later pass copies JSONSchemaProps by value, so
// the pointer survives exactly when the default does: a junctor member's default is cleared by
// sanitizeJunctorMember, and an allOf's defaults are never merged.
func survivingDefaults(s *apiextensionsv1.JSONSchemaProps, out map[*apiextensionsv1.JSON]bool) {
	if s == nil {
		return
	}
	if s.Default != nil {
		out[s.Default] = true
	}
	for _, m := range []map[string]apiextensionsv1.JSONSchemaProps{s.Properties, s.PatternProperties, s.Definitions} {
		for k := range m {
			v := m[k]
			survivingDefaults(&v, out)
		}
	}
	for _, l := range [][]apiextensionsv1.JSONSchemaProps{s.AllOf, s.AnyOf, s.OneOf} {
		for i := range l {
			survivingDefaults(&l[i], out)
		}
	}
	survivingDefaults(s.Not, out)
	if s.Items != nil {
		survivingDefaults(s.Items.Schema, out)
		for i := range s.Items.JSONSchemas {
			survivingDefaults(&s.Items.JSONSchemas[i], out)
		}
	}
	if s.AdditionalProperties != nil {
		survivingDefaults(s.AdditionalProperties.Schema, out)
	}
	if s.AdditionalItems != nil {
		survivingDefaults(s.AdditionalItems.Schema, out)
	}
	for k := range s.Dependencies {
		survivingDefaults(s.Dependencies[k].Schema, out)
	}
}
