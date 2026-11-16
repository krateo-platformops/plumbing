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
)

// DefaultContradictionError reports `default`s in an input schema that do not satisfy the subschema
// they are written in (plumbing#36). It is a CONFIG error: the chart's values.schema.json contradicts
// itself, and no change to crdgen can fix it. Match it with errors.As.
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
// the INPUT, against the input's own subschema, is what makes the blame exact: a default that fails
// here is the chart's, whatever crdgen does with it afterwards.
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
		fmt.Fprintf(&sb, "%s: default %s: %s", c.Pointer, c.Default, strings.Join(c.Reasons, ", "))
	}
	return sb.String()
}

// defaultCheck is what checkDefaults learned about one input schema.
type defaultCheck struct {
	// contradictions is non-empty when a default fails its own subschema.
	contradictions []DefaultContradiction
	// checked counts the defaults that were validated (and passed or failed).
	checked int
	// unchecked describes every default that could NOT be validated, and why. A default that cannot
	// be checked is not reported as a contradiction: the check only blames the input when it has
	// proved the input wrong.
	unchecked []string
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
// written in.
//
// Every default, not only the ones crdgen carries into the CRD: the contradiction is a property of
// the input, and an author who wrote one has a schema that disagrees with itself wherever it sits.
//
// The walk is over the RAW document rather than crdgen's parsed model because the model normalizes
// spelling (`definitions` becomes `$defs`, `dependencies` becomes `dependentSchemas`), and a pointer
// the author cannot find in their own file would be no help.
//
// The draft is the document's own `$schema`, or the validator's latest when there is none, which is
// what helm does with a chart's values.schema.json (chartutil.ValidateAgainstSingleSchema).
func checkDefaults(raw []byte) defaultCheck {
	var res defaultCheck

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		res.unchecked = append(res.unchecked, fmt.Sprintf("the document could not be read: %v", err))
		return res
	}
	sites := collectDefaultSites(doc, nil)
	if len(sites) == 0 {
		return res
	}

	c := jsonschema.NewCompiler()
	c.UseLoader(refusingLoader{})
	if err := c.AddResource(schemaDocURL, doc); err != nil {
		res.unchecked = append(res.unchecked, fmt.Sprintf("%d default(s): the document is not a schema the validator accepts: %v", len(sites), err))
		return res
	}

	for _, s := range sites {
		ptr := pointerString(s.tokens)
		sch, err := c.Compile(schemaDocURL + "#" + fragmentEscape(s.tokens))
		if err != nil {
			res.unchecked = append(res.unchecked, fmt.Sprintf("%s: %v", ptr, firstLine(err.Error())))
			continue
		}
		verr := sch.Validate(s.value)
		var ve *jsonschema.ValidationError
		if verr != nil && !errors.As(verr, &ve) {
			res.unchecked = append(res.unchecked, fmt.Sprintf("%s: %v", ptr, firstLine(verr.Error())))
			continue
		}
		res.checked++
		if verr == nil {
			continue
		}
		rendered, _ := json.Marshal(s.value)
		res.contradictions = append(res.contradictions, DefaultContradiction{
			Pointer: ptr,
			Default: rendered,
			Reasons: leafReasons(ve),
		})
	}
	sort.Slice(res.contradictions, func(i, j int) bool { return res.contradictions[i].Pointer < res.contradictions[j].Pointer })
	return res
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
// whose input defaults did not contradict their schemas.
//
// It does not claim the refusal is a crdgen bug. The input can still cause one in ways this check
// does not cover: an author's own x-kubernetes-validations rule over its CEL cost budget, or a
// `pattern` that is valid ECMA-262 but not RE2. The apiserver's field paths, which follow, are what
// locate it.
func refusalAttribution(checks ...defaultCheck) string {
	checked := 0
	var unchecked []string
	for _, c := range checks {
		checked += c.checked
		unchecked = append(unchecked, c.unchecked...)
	}
	if len(unchecked) > 0 {
		return fmt.Sprintf("%d default(s) in the input could not be checked against their own schema, "+
			"so a contradictory default is not ruled out: %s", len(unchecked), strings.Join(unchecked, "; "))
	}
	if checked == 0 {
		return "the input has no defaults, so a contradictory default is not the cause"
	}
	return fmt.Sprintf("all %d default(s) in the input satisfy their own schema, so a contradictory default is not the cause", checked)
}
