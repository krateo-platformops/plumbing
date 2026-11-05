package crdgen

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// #82. observedGeneration is injected beside conditions for a MANAGED resource, type-aware.

func crdSchemaRoot(t *testing.T, crd []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(crd, &doc); err != nil {
		t.Fatal(err)
	}
	v := doc["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)
	return v["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)
}

func schemaField(root map[string]any, top, name string) (map[string]any, bool) {
	props, _ := root[top].(map[string]any)["properties"].(map[string]any)
	f, ok := props[name].(map[string]any)
	return f, ok
}

func ogWarnings(ws []string) []string {
	var out []string
	for _, w := range ws {
		if strings.HasPrefix(w, "status.observedGeneration") {
			out = append(out, w)
		}
	}
	return out
}

const ogSpec = `{"type":"object","properties":{"observedGeneration":{"type":"string"},"name":{"type":"string"}}}`

func TestObservedGeneration_TypeAware(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      string
		wantType    string
		wantFormat  string
		wantDesc    string // "" = do not check
		wantWarning bool
	}{
		{"absent: injected as int64", `{"type":"object","properties":{"phase":{"type":"string"}}}`, "integer", "int64", "", false},
		{"no status schema at all: injected", ``, "integer", "int64", "", false},
		{"declared integer: left exactly as declared", `{"type":"object","properties":{"observedGeneration":{"type":"integer","description":"authored"}}}`, "integer", "", "authored", false},
		{"declared string: kept, and warned", `{"type":"object","properties":{"observedGeneration":{"type":"string","description":"vendor field"}}}`, "string", "", "vendor field", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{Group: "x.io", Version: "v1", Kind: "Port", SpecSchema: []byte(ogSpec), Managed: true}
			if tc.status != "" {
				opts.StatusSchema = []byte(tc.status)
			}
			crd, ws, err := GenerateWithWarnings(opts)
			if err != nil {
				t.Fatal(err)
			}
			og, ok := schemaField(crdSchemaRoot(t, crd), "status", "observedGeneration")
			if !ok {
				t.Fatal("status.observedGeneration is not declared, so a controller's writes to it are pruned (#82)")
			}
			if og["type"] != tc.wantType {
				t.Errorf("type = %v, want %v", og["type"], tc.wantType)
			}
			if tc.wantFormat != "" && og["format"] != tc.wantFormat {
				t.Errorf("format = %v, want %v", og["format"], tc.wantFormat)
			}
			if tc.wantDesc != "" && og["description"] != tc.wantDesc {
				t.Errorf("the author's declaration was replaced: description = %v, want %q", og["description"], tc.wantDesc)
			}
			if got := ogWarnings(ws); (len(got) > 0) != tc.wantWarning {
				t.Errorf("warning present = %v, want %v: %v", len(got) > 0, tc.wantWarning, got)
			}
			if _, ok := schemaField(crdSchemaRoot(t, crd), "status", "conditions"); !ok {
				t.Error("conditions must still be injected beside it")
			}
		})
	}
}

// The controls. A SPEC property of the same name is the author's and untouched; a non-managed
// resource gets neither convention field.
func TestObservedGeneration_Controls(t *testing.T) {
	crd, _, err := GenerateWithWarnings(Options{Group: "x.io", Version: "v1", Kind: "Port", SpecSchema: []byte(ogSpec), Managed: true})
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := schemaField(crdSchemaRoot(t, crd), "spec", "observedGeneration"); f["type"] != "string" {
		t.Errorf("a spec property named observedGeneration was changed: %v", f)
	}

	crd, ws, err := GenerateWithWarnings(Options{Group: "x.io", Version: "v1", Kind: "Port", SpecSchema: []byte(ogSpec),
		StatusSchema: []byte(`{"type":"object","properties":{"phase":{"type":"string"}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := schemaField(crdSchemaRoot(t, crd), "status", "observedGeneration"); ok {
		t.Error("a NON-managed resource got observedGeneration injected; the convention fields are for managed resources only")
	}
	if got := ogWarnings(ws); len(got) != 0 {
		t.Errorf("non-managed resource warned: %v", got)
	}
}
