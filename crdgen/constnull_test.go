package crdgen

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// plumbing#227: `const: null` ("only null") used to parse as no const, so the field accepted any
// value with no warning. It is enforced where a CRD can hold it exactly, and reported everywhere
// else; it never vanishes.
func TestConstNullIsEnforcedOrReported(t *testing.T) {
	const spec = `{"type":"object","properties":{
	  "typed":{"type":"string","const":null},
	  "typedInt":{"type":"integer","const":null},
	  "nullable":{"type":["string","null"],"const":null},
	  "nullableOAS30":{"type":"string","nullable":true,"const":null},
	  "untyped":{"const":null},
	  "control":{"type":"string","const":"a"},
	  "negated":{"type":"string","not":{"const":null}},
	  "gatedIf":{"type":"object","properties":{"mode":{"type":"string"},"x":{"type":"string"}},
	    "if":{"properties":{"mode":{"const":null}}},"then":{"required":["x"]}},
	  "gatedThen":{"type":"object","properties":{"mode":{"type":"string"},"x":{"type":"string"}},
	    "if":{"properties":{"mode":{"const":"a"}},"required":["mode"]},"then":{"properties":{"x":{"const":null}}}},
	  "gatedSelf":{"type":"object","properties":{"mode":{"type":"string"}},
	    "if":{"properties":{"mode":{"const":"a"}},"required":["mode"]},"then":{"const":null}}}}`
	out, ws, err := GenerateWithDiagnostics(Options{Group: "test.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(spec)})
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	warned := func(name, substr string) bool {
		for _, w := range ws {
			if w.Kind == KindConstraintDropped && w.Path == "spec."+name && strings.Contains(w.Message, substr) {
				return true
			}
		}
		return false
	}
	// apiextensionsv1.JSON decodes a null literal as an empty Raw (and encodes it back as null).
	enumOf := func(p apiextensionsv1.JSONSchemaProps) string {
		var b []string
		for _, e := range p.Enum {
			if len(e.Raw) == 0 {
				b = append(b, "null")
				continue
			}
			b = append(b, string(e.Raw))
		}
		return strings.Join(b, ",")
	}

	// Enforced, exactly: a typed, non-nullable field.
	for _, name := range []string{"typed", "typedInt"} {
		if got := enumOf(props[name]); got != "null" {
			t.Errorf("%s: enum = %q, want null (enforced)", name, got)
		}
		if warned(name, "") {
			t.Errorf("%s warned, but nothing of it was dropped", name)
		}
	}
	// Control: a non-null const is the single-value enum it always was.
	if got := enumOf(props["control"]); got != `"a"` {
		t.Errorf("control: enum = %q", got)
	}

	// Reported: where the CRD can't hold it.
	// Both spellings of nullable (JSON Schema's type list, OAS 3.0's `nullable: true`): nullable wins
	// over the type, since enum [null] on a nullable field refuses null itself.
	for _, name := range []string{"nullable", "nullableOAS30"} {
		if p := props[name]; len(p.Enum) != 0 || !p.Nullable || !warned(name, "const: null on a nullable field") {
			t.Errorf("%s: enum %q nullable %v, warned %v", name, enumOf(p), p.Nullable, warned(name, "const: null"))
		}
	}
	if p := props["untyped"]; p.XPreserveUnknownFields == nil || len(p.Enum) != 0 || !warned("untyped", "const: null without a typed node") {
		t.Errorf("untyped: %+v, warned %v", p, warned("untyped", "const: null"))
	}
	// Inside a not, dropping it would make the not refuse more: the whole not is dropped, and said so.
	if p := props["negated"]; p.Not != nil || !warned("negated", "const: null") {
		t.Errorf("negated: not %+v, warned %v", p.Not, warned("negated", "const: null"))
	}
	// In an if, dropping it would widen the condition (then would bind every object): the
	// if/then is dropped, and said so, rather than refusing objects the schema admits.
	if p := props["gatedIf"]; len(p.XValidations) != 0 || !warned("gatedIf", "if condition not expressible") {
		t.Errorf("gatedIf: rules %v, warned %v", p.XValidations, warned("gatedIf", "if condition"))
	}
	// A whole-object const: null in a then would otherwise add no rule and no warning.
	if p := props["gatedSelf"]; len(p.XValidations) != 0 || !warned("gatedSelf", "then not expressible in CEL") {
		t.Errorf("gatedSelf: rules %v, warned %v", p.XValidations, warned("gatedSelf", "then not expressible"))
	}
	// In a then, the clause is dropped and named.
	if !warned("gatedThen", "const: null not expressible in CEL") {
		t.Errorf("gatedThen: the then clause's const: null was dropped without a warning: %v", ws)
	}
}
