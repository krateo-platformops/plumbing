package crdgen

// A `type: ["integer","string"]` union is the structural int-or-string node written in JSON
// Schema's own vocabulary (#77).
//
// WHAT WENT WRONG WITHOUT THIS. primaryType kept the first non-null member and dropped the rest in
// silence, so `["integer","string"]` became `type: integer` and a quoted value was refused by the
// apiserver while the source schema accepted it. Measured by two consumers before this landed:
// krateo-platformops/installer's own Installer CRD refused
// `OASGEN_PROVIDER_MAX_RECONCILE_RATE: "10"`, which is how an env value is normally written in a
// values file; and krateo-platformops/frontend carried a post-pass re-reading the source schema to
// repair five widget fields, without which `Image.width: "100%"` and `Flex.gap: 8` were both
// unappliable.
//
// THE TRAP THIS FIX HAD TO AVOID, and it is why the test for `pattern` and `enum` surviving is the
// most important one here. The XIntOrString branch used to `return out` early, carrying only
// XIntOrString and Title. Routing a union into it as written would have swapped one silent loss
// (a dropped type member) for another (dropped scalar keywords) — caught by the oasgen-provider
// session before the patch, not after.

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func iosGenerate(t *testing.T, src string) map[string]apiextensionsv1.JSONSchemaProps {
	t.Helper()
	out, warnings, err := GenerateWithWarnings(Options{
		Group: "test.krateo.io", Version: "v1", Kind: "Ios",
		SpecSchema: []byte(src), StatusSchema: []byte(defaultStatusSchema),
	})
	if err != nil {
		t.Fatalf("generate (the apiserver's own gate rejected it): %v\nwarnings: %v", err, warnings)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
}

func TestIntOrStringUnion(t *testing.T) {
	p := iosGenerate(t, `{
	  "type": "object",
	  "properties": {
	    "width":     { "type": ["integer", "string"] },
	    "gap":       { "type": ["string", "integer"] },
	    "maybe":     { "type": ["integer", "string", "null"] },
	    "plainInt":  { "type": ["integer"] },
	    "nullableS": { "type": ["string", "null"] }
	  }
	}`)

	// The union, in both orders — a set, not a sequence.
	for _, name := range []string{"width", "gap"} {
		if !p[name].XIntOrString {
			t.Errorf("%s: type union [integer,string] must become x-kubernetes-int-or-string", name)
		}
		if p[name].Type != "" {
			t.Errorf("%s: a structural int-or-string node carries no type, got %q", name, p[name].Type)
		}
	}

	// `null` alongside the pair is still the pair, plus nullable.
	if !p["maybe"].XIntOrString || !p["maybe"].Nullable {
		t.Errorf("maybe: [integer,string,null] must be int-or-string AND nullable, got "+
			"XIntOrString=%v Nullable=%v", p["maybe"].XIntOrString, p["maybe"].Nullable)
	}

	// CONTROL, and core-provider asked for exactly this one: a single-member array must be
	// untouched. Without it, a fix that routes too much into the XIntOrString branch passes every
	// assertion above.
	if p["plainInt"].Type != "integer" || p["plainInt"].XIntOrString {
		t.Errorf("plainInt: a single-member type array must stay a plain typed node, got type=%q "+
			"XIntOrString=%v", p["plainInt"].Type, p["plainInt"].XIntOrString)
	}

	// The other control: ["T","null"] was ALREADY correct and must not be caught by this. A fix
	// that fired here would flag correct code, which is a fix someone switches off.
	if p["nullableS"].Type != "string" || !p["nullableS"].Nullable || p["nullableS"].XIntOrString {
		t.Errorf("nullableS: [string,null] must stay a nullable string, got type=%q nullable=%v "+
			"XIntOrString=%v", p["nullableS"].Type, p["nullableS"].Nullable,
			p["nullableS"].XIntOrString)
	}
}

// THE ARM THAT MAKES THIS A FIX RATHER THAN A DIFFERENT LOSS.
func TestIntOrStringKeepsItsScalarKeywords(t *testing.T) {
	p := iosGenerate(t, `{
	  "type": "object",
	  "properties": {
	    "fromUnion":    { "type": ["integer", "string"], "pattern": "^[0-9]+%?$", "maxLength": 8 },
	    "fromExtension":{ "x-kubernetes-int-or-string": true, "pattern": "^[0-9]+%?$", "maxLength": 8 },
	    "withEnum":     { "type": ["integer", "string"], "enum": [1, "auto"] },
	    "withDefault":  { "type": ["integer", "string"], "default": "auto" }
	  }
	}`)

	// Both routes in, same guarantee out. `fromExtension` is the REGRESSION the early return
	// caused: an explicit x-kubernetes-int-or-string with a pattern lost it long before unions
	// were considered.
	for _, name := range []string{"fromUnion", "fromExtension"} {
		if !p[name].XIntOrString {
			t.Fatalf("%s: not int-or-string", name)
		}
		if p[name].Pattern != "^[0-9]+%?$" {
			t.Errorf("%s: pattern dropped, got %q — the apiserver applies it to the string branch "+
				"and leaves integers alone, so dropping it loses the author's constraint",
				name, p[name].Pattern)
		}
		if p[name].MaxLength == nil || *p[name].MaxLength != 8 {
			t.Errorf("%s: maxLength dropped, got %v", name, p[name].MaxLength)
		}
	}
	if len(p["withEnum"].Enum) != 2 {
		t.Errorf("withEnum: enum dropped, got %v", p["withEnum"].Enum)
	}
	if p["withDefault"].Default == nil {
		t.Error("withDefault: default dropped")
	}
}

// A union this transpiler cannot express structurally still keeps its first member — unchanged
// behaviour — but it no longer does so in silence. The warning has somewhere to go since
// GenerateWithWarnings; before that it would have been a diagnostic into a dead sink.
func TestUnexpressibleUnionWarnsAndKeepsTheFirstMember(t *testing.T) {
	out, warnings, err := GenerateWithWarnings(Options{
		Group: "test.krateo.io", Version: "v1", Kind: "Ios",
		SpecSchema: []byte(`{
		  "type": "object",
		  "properties": {
		    "three": { "type": ["boolean", "integer", "string"] },
		    "pair":  { "type": ["array", "object"] }
		  }
		}`),
		StatusSchema: []byte(defaultStatusSchema),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	if p["three"].Type != "boolean" {
		t.Errorf("three: the first member must still be kept, got %q", p["three"].Type)
	}

	var sawThree, sawPair bool
	for _, w := range warnings {
		if strings.Contains(w, "three") && strings.Contains(w, "dropping") {
			sawThree = true
		}
		if strings.Contains(w, "pair") && strings.Contains(w, "dropping") {
			sawPair = true
		}
	}
	if !sawThree || !sawPair {
		t.Errorf("both unexpressible unions must warn; three=%v pair=%v\nwarnings: %v",
			sawThree, sawPair, warnings)
	}

	// And the shapes that must NOT warn, so the diagnostic stays worth reading.
	_, quiet, err := GenerateWithWarnings(Options{
		Group: "test.krateo.io", Version: "v1", Kind: "Ios",
		SpecSchema: []byte(`{
		  "type": "object",
		  "properties": {
		    "ios":      { "type": ["integer", "string"] },
		    "nullable": { "type": ["string", "null"] },
		    "plain":    { "type": "string" }
		  }
		}`),
		StatusSchema: []byte(defaultStatusSchema),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, w := range quiet {
		if strings.Contains(w, "dropping") {
			t.Errorf("no member is dropped by any of these shapes, but got: %q", w)
		}
	}
}
