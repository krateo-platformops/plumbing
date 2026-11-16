package crdgen

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/crdgen/schemas"
)

// plumbing#36: a `default` that does not satisfy the schema it is written in is the chart's error,
// and generation says so with the pointer the author can find in their own file. The issue's
// reproducer, verbatim.
func TestDefaultContradictingItsOwnSchemaIsAConfigError(t *testing.T) {
	_, err := Generate(Options{
		Group: "composition.krateo.io", Version: "0.1.1", Kind: "Mongodb",
		SpecSchema: []byte(`{"type":"object","properties":{"security":{"type":"object",
      "default":{},"properties":{"authentication":{"type":"object"}},"required":["authentication"]}}}`),
		Managed: true,
	})
	var dc *DefaultContradictionError
	if !errors.As(err, &dc) {
		t.Fatalf("want a *DefaultContradictionError, got %T: %v", err, err)
	}
	if dc.Schema != "spec schema" || len(dc.Contradictions) != 1 {
		t.Fatalf("want one contradiction in the spec schema, got %+v", dc)
	}
	c := dc.Contradictions[0]
	if c.Pointer != "#/properties/security" || string(c.Default) != "{}" {
		t.Errorf("pointer/default = %q %s, want #/properties/security {}", c.Pointer, c.Default)
	}
	if len(c.Reasons) != 1 || !strings.Contains(c.Reasons[0], "authentication") {
		t.Errorf("reasons %q do not name the missing property", c.Reasons)
	}
	if msg := err.Error(); strings.Contains(msg, "is a crdgen bug") || !strings.Contains(msg, "config error") {
		t.Errorf("message blames the wrong party: %s", msg)
	}
}

// A default that satisfies its subschema changes nothing: the control for every case below.
func TestDefaultsThatSatisfyTheirSchemaStillGenerate(t *testing.T) {
	crd := generateV1(t, `{"type":"object","properties":{
		"security":{"type":"object","default":{"authentication":{}},"required":["authentication"],
			"properties":{"authentication":{"type":"object"}}},
		"replicas":{"type":"integer","minimum":1,"default":3},
		"mode":{"type":"string","enum":["a","b"],"default":"a"}}}`)
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if d := spec.Properties["replicas"].Default; d == nil || string(d.Raw) != "3" {
		t.Errorf("replicas default lost: %v", d)
	}
}

func TestDefaultContradictionCases(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		want         []string // pointers, sorted; nil means generation succeeds
		reason       string   // a substring of the first contradiction's reasons
	}{
		{name: "type mismatch",
			schema: `{"type":"object","properties":{"replicas":{"type":"integer","default":"three"}}}`,
			want:   []string{"#/properties/replicas"}, reason: "want integer"},
		{name: "enum miss",
			schema: `{"type":"object","properties":{"mode":{"type":"string","enum":["a","b"],"default":"c"}}}`,
			want:   []string{"#/properties/mode"}, reason: "must be one of 'a', 'b'"},
		{name: "deep: an item's default, under items",
			schema: `{"type":"object","properties":{"l":{"type":"array","items":{"type":"object","default":{},"required":["x"],"properties":{"x":{"type":"string"}}}}}}`,
			want:   []string{"#/properties/l/items"}, reason: "'x'"},
		{name: "every contradiction is reported, sorted",
			schema: `{"type":"object","properties":{"b":{"type":"integer","default":"x"},"a":{"type":"string","default":1}}}`,
			want:   []string{"#/properties/a", "#/properties/b"}, reason: "want string"},
		{name: "legacy definitions keep their spelling",
			schema: `{"type":"object","definitions":{"Tls":{"type":"object","default":{},"required":["cert"],"properties":{"cert":{"type":"string"}}}},"properties":{"tls":{"$ref":"#/definitions/Tls"}}}`,
			want:   []string{"#/definitions/Tls"}, reason: "'cert'"},
		{name: "a property named default is a schema; the default keyword's value is data",
			schema: `{"type":"object","properties":{"default":{"type":"string","default":7},"x":{"type":"object","default":{"default":{"type":"integer","default":"not checked: this is data"}}}}}`,
			want:   []string{"#/properties/default"}, reason: "want string"},
		{name: "enum and const values are data",
			schema: `{"type":"object","properties":{"x":{"enum":[{"default":1,"type":"string"}]},"y":{"const":{"default":"z","type":"integer"}}}}`},
		{name: "property names are escaped in the pointer",
			schema: `{"type":"object","properties":{"a/b c~%":{"type":"integer","default":"x"}}}`,
			want:   []string{"#/properties/a~1b c~0%"}, reason: "want integer"},
		{name: "draft-04 boolean exclusiveMinimum is read as draft-04",
			schema: `{"$schema":"http://json-schema.org/draft-04/schema#","type":"object","properties":{"n":{"type":"number","minimum":1,"exclusiveMinimum":true,"default":1}}}`,
			want:   []string{"#/properties/n"}, reason: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(tc.schema)})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("want generation to succeed, got %v", err)
				}
				return
			}
			var dc *DefaultContradictionError
			if !errors.As(err, &dc) {
				t.Fatalf("want a *DefaultContradictionError, got %T: %v", err, err)
			}
			var got []string
			for _, c := range dc.Contradictions {
				got = append(got, c.Pointer)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("pointers = %q, want %q", got, tc.want)
			}
			if r := strings.Join(dc.Contradictions[0].Reasons, ", "); !strings.Contains(r, tc.reason) {
				t.Errorf("reasons %q lack %q", r, tc.reason)
			}
		})
	}
}

// A contradicting default crdgen does NOT carry into the CRD never reached the apiserver, so the CRD
// installed; failing generation for it would break a CRD that works today. It is a warning, with
// the same pointer and reason, and the CRD is the one main generates (decided by Diego, 2026-11-16).
// Each case was measured to generate on v1.89.0.
func TestUncarriedContradictionsWarnAndGenerate(t *testing.T) {
	for _, tc := range []struct{ name, schema, pointer, reason string }{
		{"anyOf member",
			`{"type":"object","properties":{"x":{"anyOf":[{"type":"string","default":5},{"type":"null"}]}}}`,
			"#/properties/x/anyOf/0", "want string"},
		{"oneOf member",
			`{"type":"object","properties":{"x":{"type":"object","oneOf":[{"required":["a"],"default":{}},{"required":["b"]}],"properties":{"a":{"type":"string"},"b":{"type":"string"}}}}}`,
			"#/properties/x/oneOf/0", "'a'"},
		{"allOf member",
			`{"type":"object","properties":{"x":{"allOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"default":{}}]}}}`,
			"#/properties/x/allOf/0", "'a'"},
		{"a default beside a $ref, checked against the target",
			`{"type":"object","$defs":{"Port":{"type":"integer","maximum":65535}},"properties":{"port":{"$ref":"#/$defs/Port","default":70000}}}`,
			"#/properties/port", "want 65,535"},
		{"patternProperties",
			`{"type":"object","properties":{"labels":{"type":"object","patternProperties":{"^x-":{"type":"integer","default":"nope"}}}}}`,
			"#/properties/labels/patternProperties/^x-", "want integer"},
		{"under not",
			`{"type":"object","properties":{"x":{"type":"string","not":{"type":"string","enum":["a"],"default":"b"}}}}`,
			"#/properties/x/not", "value must be 'a'"},
		{"unreferenced $defs entry",
			`{"type":"object","$defs":{"Unused":{"type":"integer","default":"x"}},"properties":{"a":{"type":"string"}}}`,
			"#/$defs/Unused", "want integer"},
		{"draft-7 dependencies: a schema value is descended, a name list is not",
			`{"type":"object","properties":{"a":{"type":"string"}},"dependencies":{"a":{"properties":{"b":{"type":"string","default":1}}},"c":["a"]}}`,
			"#/dependencies/a/properties/b", "want string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, warns, err := GenerateWithDiagnostics(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(tc.schema)})
			if err != nil {
				t.Fatalf("an uncarried default must not fail generation: %v", err)
			}
			var got []Warning
			for _, w := range warns {
				if w.Kind == KindDefaultContradictsSchema {
					got = append(got, w)
				}
			}
			if len(got) != 1 || got[0].Path != tc.pointer || !strings.Contains(got[0].Message, tc.reason) {
				t.Fatalf("want one DefaultContradictsSchema warning at %s mentioning %q, got %+v", tc.pointer, tc.reason, got)
			}
			if got[0].Kind.LosesEnforcement() {
				t.Error("the CRD loses nothing to a self-contradicting default it does not carry")
			}
		})
	}
}

// One schema with both: the error names only the carried default, and the uncarried one is still
// reported as a warning beside it.
func TestCarriedFailsAndUncarriedWarnsTogether(t *testing.T) {
	_, warns, err := GenerateWithDiagnostics(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe",
		SpecSchema: []byte(`{"type":"object","properties":{"n":{"type":"integer","default":"x"},"o":{"anyOf":[{"type":"string","default":5},{"type":"null"}]}}}`)})
	var dc *DefaultContradictionError
	if !errors.As(err, &dc) || len(dc.Contradictions) != 1 || dc.Contradictions[0].Pointer != "#/properties/n" {
		t.Fatalf("want an error naming #/properties/n only, got %v", err)
	}
	found := false
	for _, w := range warns {
		found = found || (w.Kind == KindDefaultContradictsSchema && w.Path == "#/properties/o/anyOf/0")
	}
	if !found {
		t.Errorf("the uncarried contradiction is missing from the warnings: %+v", warns)
	}
}

func TestStatusSchemaDefaultsAreCheckedToo(t *testing.T) {
	_, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe",
		SpecSchema:   []byte(`{"type":"object","properties":{"a":{"type":"string"}}}`),
		StatusSchema: []byte(`{"type":"object","properties":{"phase":{"type":"string","enum":["Ready"],"default":"Pending"}}}`)})
	var dc *DefaultContradictionError
	if !errors.As(err, &dc) || dc.Schema != "status schema" || dc.Contradictions[0].Pointer != "#/properties/phase" {
		t.Fatalf("want a status-schema contradiction at #/properties/phase, got %v", err)
	}
}

// A default the check cannot validate is never reported as a contradiction, and nothing outside
// the document is read: a $ref to a file or a URL is refused by the loader, not followed.
//
// The file case is a real, valid schema on disk: a loader that followed it would check the default
// (checked=1) instead of reporting it unchecked, so the test sees whether the refusal is in place.
func TestUncheckableDefaultsAreNotBlamed(t *testing.T) {
	local := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(local, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"file://" + filepath.ToSlash(local), "https://example.invalid/schema.json"} {
		t.Run(ref, func(t *testing.T) {
			schema := `{"type":"object","properties":{"r":{"type":"object","default":{},"properties":{"x":{"$ref":"` + ref + `"}}}}}`
			parsed, err := schemas.FromJSONReader(strings.NewReader(schema))
			if err != nil {
				t.Fatal(err)
			}
			chk := checkDefaults("spec schema", []byte(schema), parsed)
			if len(chk.contradictions) != 0 || len(chk.passed) != 0 || len(chk.unchecked) != 1 {
				t.Fatalf("want one unchecked default and no contradiction, got %+v", chk)
			}
			if _, ok := chk.unchecked["#/properties/r"]; !ok {
				t.Errorf("unchecked does not name the default: %v", chk.unchecked)
			}
		})
	}
}

// When the apiserver refuses a CRD for a reason other than a default, the message no longer claims
// to know it is a crdgen bug, and says what the default check established. The refusal here is the
// input's own: a hand-written CEL rule that does not compile, carried through as written.
func TestApiserverRefusalNoLongerBlamesCrdgen(t *testing.T) {
	badRule := `"x-kubernetes-validations":[{"rule":"self.("}]`
	for _, tc := range []struct{ name, schema, want string }{
		{"defaults all checked",
			`{"type":"object","properties":{"a":{"type":"string","default":"x",` + badRule + `}}}`,
			"all 1 default(s) carried into the CRD satisfy their own schema"},
		{"no defaults",
			`{"type":"object","properties":{"a":{"type":"string",` + badRule + `}}}`,
			"no default from the input is carried into the CRD"},
		{"a default could not be checked",
			`{"type":"object","properties":{"a":{"type":"string",` + badRule + `},"r":{"type":"object","default":{},"properties":{"x":{"$ref":"https://example.invalid/s.json"}}}}}`,
			"1 default(s) carried into the CRD could not be checked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(tc.schema)})
			if err == nil {
				t.Fatal("want the apiserver's gate to refuse the CRD")
			}
			msg := err.Error()
			if !strings.Contains(msg, "failed the apiserver's own validation") || !strings.Contains(msg, tc.want) {
				t.Errorf("message lacks %q: %s", tc.want, msg)
			}
			if strings.Contains(msg, "crdgen bug") {
				t.Errorf("message still blames crdgen: %s", msg)
			}
			var dc *DefaultContradictionError
			if errors.As(err, &dc) {
				t.Errorf("not a default contradiction, yet typed as one: %v", dc)
			}
		})
	}
}

// The spec and status schemas are checked separately, and each one's carried defaults are counted
// against its own document: a spec default is never "unlocated" in the status schema.
func TestRefusalAttributionCountsBothSchemas(t *testing.T) {
	_, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe",
		SpecSchema:   []byte(`{"type":"object","properties":{"a":{"type":"string","default":"x","x-kubernetes-validations":[{"rule":"self.("}]}}}`),
		StatusSchema: []byte(`{"type":"object","properties":{"phase":{"type":"string","default":"Pending"}}}`)})
	if err == nil || !strings.Contains(err.Error(), "all 2 default(s) carried into the CRD satisfy their own schema") {
		t.Fatalf("want both carried defaults counted as checked, got %v", err)
	}
}

// A carried default whose node the index cannot locate is reported as unknown, never as checked.
// No schema in the suite produces one (every node copyDefault sees is indexed), so this pins the
// guard directly.
func TestVerdictReportsACarriedNodeItCannotLocate(t *testing.T) {
	c := defaultCheck{schema: "spec schema", pointerOf: map[*schemas.Type]string{}}
	v := c.verdict(map[*schemas.Type]bool{{}: true})
	if len(v.carriedUnknown) != 1 || v.carriedPassed != 0 || v.err != nil {
		t.Fatalf("want one unknown carried default, got %+v", v)
	}
	if got := refusalAttribution(v); !strings.Contains(got, "could not be checked") {
		t.Errorf("attribution claims more than it knows: %s", got)
	}
}
