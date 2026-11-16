package crdgen

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		{name: "a default beside a $ref is checked against the target",
			schema: `{"type":"object","$defs":{"Port":{"type":"integer","maximum":65535}},"properties":{"port":{"$ref":"#/$defs/Port","default":70000}}}`,
			want:   []string{"#/properties/port"}, reason: "want 65,535"},
		{name: "a property named default is a schema; the default keyword's value is data",
			schema: `{"type":"object","properties":{"default":{"type":"string","default":7},"x":{"type":"object","default":{"default":{"type":"integer","default":"not checked: this is data"}}}}}`,
			want:   []string{"#/properties/default"}, reason: "want string"},
		{name: "a junction member's default is checked against the member",
			schema: `{"type":"object","properties":{"x":{"anyOf":[{"type":"string","default":5},{"type":"null"}]}}}`,
			want:   []string{"#/properties/x/anyOf/0"}, reason: "want string"},
		{name: "draft-7 dependencies: a schema value is descended, a name list is not",
			schema: `{"type":"object","properties":{"a":{"type":"string"}},"dependencies":{"a":{"properties":{"b":{"type":"string","default":1}}},"c":["a"]}}`,
			want:   []string{"#/dependencies/a/properties/b"}, reason: "want string"},
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
			chk := checkDefaults([]byte(schema))
			if len(chk.contradictions) != 0 || chk.checked != 0 || len(chk.unchecked) != 1 {
				t.Fatalf("want one unchecked default and no contradiction, got %+v", chk)
			}
			if !strings.Contains(chk.unchecked[0], "#/properties/r") {
				t.Errorf("unchecked entry does not name the default: %q", chk.unchecked[0])
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
			"all 1 default(s) in the input satisfy their own schema"},
		{"no defaults",
			`{"type":"object","properties":{"a":{"type":"string",` + badRule + `}}}`,
			"the input has no defaults"},
		{"a default could not be checked",
			`{"type":"object","properties":{"a":{"type":"string",` + badRule + `},"r":{"type":"object","default":{},"properties":{"x":{"$ref":"https://example.invalid/s.json"}}}}}`,
			"1 default(s) in the input could not be checked"},
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
