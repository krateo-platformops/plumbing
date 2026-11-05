package crdgen

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// plumbing#84. oasgen-provider's shape: an optional object `properties` whose children carry
// defaults. By default it is stamped `default: {}` AND reported; with NoParentDefaults it is neither.
//
// `aaa` is there for the ORDER: the walk collects nested stamps before top-level ones, so without a
// top-level parent that sorts first, collection order would happen to equal sorted order and an
// unsorted census could not be told apart (a surviving mutant showed exactly that).
const vpcShape = `{"type":"object","properties":{
  "name":{"type":"string"},
  "aaa":{"type":"object","properties":{"y":{"type":"string","default":"z"}}},
  "properties":{"type":"object","properties":{
    "default":{"type":"boolean","default":true},
    "preset":{"type":"boolean","default":false}}},
  "list":{"type":"array","items":{"type":"object","properties":{
    "opts":{"type":"object","properties":{"x":{"type":"integer","default":1}}}}}}}}`

// specProp returns the generated CRD's schema for spec.<path...>, following `items` on "[*]".
func specProp(t *testing.T, crd []byte, path ...string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(crd, &doc); err != nil {
		t.Fatal(err)
	}
	v := doc["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)
	cur := v["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)["spec"].(map[string]any)
	for _, p := range path {
		if p == "[*]" {
			cur = cur["items"].(map[string]any)
			continue
		}
		cur = cur["properties"].(map[string]any)[p].(map[string]any)
	}
	return cur
}

func stampWarnings(ws []string) []string {
	var out []string
	for _, w := range ws {
		if strings.Contains(w, "given default {}") {
			out = append(out, w)
		}
	}
	return out
}

func TestParentDefaults_StampedAndReportedByDefault(t *testing.T) {
	crd, ws, err := GenerateWithWarnings(Options{Group: "x.io", Version: "v1", Kind: "Vpc", SpecSchema: []byte(vpcShape)})
	if err != nil {
		t.Fatal(err)
	}
	if specProp(t, crd, "properties")["default"] == nil {
		t.Fatal("setup: the default behaviour no longer stamps spec.properties, so this test proves nothing")
	}
	got := stampWarnings(ws)
	// Paths in generated-CRD form, sorted, one per stamp, including one under an array.
	want := []string{"spec.aaa:", "spec.list[*].opts:", "spec.properties:"}
	if len(got) != len(want) {
		t.Fatalf("want one warning per stamped parent %v, got %v", want, got)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("warning %d = %q, want it to start with the CRD path %q", i, got[i], want[i])
		}
	}
	if specProp(t, crd, "list", "[*]", "opts")["default"] == nil {
		t.Error("the stamp under an array item was reported but not made")
	}
}

func TestParentDefaults_NoParentDefaultsStampsAndReportsNothing(t *testing.T) {
	crd, ws, err := GenerateWithWarnings(Options{Group: "x.io", Version: "v1", Kind: "Vpc", SpecSchema: []byte(vpcShape), NoParentDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := specProp(t, crd, "properties")["default"]; d != nil {
		t.Errorf("NoParentDefaults still stamped spec.properties default=%v", d)
	}
	if d := specProp(t, crd, "list", "[*]", "opts")["default"]; d != nil {
		t.Errorf("NoParentDefaults still stamped spec.list[*].opts default=%v", d)
	}
	if got := stampWarnings(ws); len(got) != 0 {
		t.Errorf("nothing was stamped, so nothing may be reported: %v", got)
	}
	// The leaf defaults themselves are the schema's own and stay: the option governs the PARENT stamp only.
	if specProp(t, crd, "properties", "default")["default"] == nil {
		t.Error("NoParentDefaults removed a leaf default the schema declared; it must only skip the parent stamp")
	}
}

// The control: no nested defaults means no stamp and no warning, with the option on or off.
func TestParentDefaults_NoNestedDefaultNeverStamps(t *testing.T) {
	plain := strings.NewReplacer(`,"default":true`, ``, `,"default":false`, ``, `,"default":1`, ``, `,"default":"z"`, ``).Replace(vpcShape)
	for _, off := range []bool{false, true} {
		crd, ws, err := GenerateWithWarnings(Options{Group: "x.io", Version: "v1", Kind: "Vpc", SpecSchema: []byte(plain), NoParentDefaults: off})
		if err != nil {
			t.Fatal(err)
		}
		if d := specProp(t, crd, "properties")["default"]; d != nil {
			t.Errorf("NoParentDefaults=%v: stamped a parent with no defaulted descendant: %v", off, d)
		}
		if got := stampWarnings(ws); len(got) != 0 {
			t.Errorf("NoParentDefaults=%v: reported a stamp that was not made: %v", off, got)
		}
	}
}
