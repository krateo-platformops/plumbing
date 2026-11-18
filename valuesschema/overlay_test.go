package valuesschema

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
)

// The overlay's rules, on the `required` fixture: svc's coalesced values are its values.yaml with
// the parent's `svc:` block merged over (port 8080 -> 8443).
func TestInfer_OverlaysSubchartDefaults(t *testing.T) {
	r := infer(t, "required")
	var doc map[string]any
	if err := json.Unmarshal(r.Schema, &doc); err != nil {
		t.Fatal(err)
	}
	get := func(keys ...string) any {
		v, _ := dig(doc, keys...)
		return v
	}
	if got := get("properties", "svc", "properties", "foo", "default"); got != "bar" {
		t.Errorf("svc.foo default = %v, want bar (the subchart's values.yaml fills the required key)", got)
	}
	if got := get("properties", "svc", "properties", "port", "default"); got != float64(8443) {
		t.Errorf("svc.port default = %v, want 8443: the PARENT's block wins, and it replaces the authored 80", got)
	}
	if got := get("properties", "svc", "required"); got == nil {
		t.Errorf("svc lost its authored required: the overlay must keep constraints")
	}

	want := map[string]struct {
		kind   FindingKind
		detail string
	}{
		"svc.foo":         {KindDefaultOverlaid, ""},
		"svc.port":        {KindDefaultOverlaid, "replaced the authored default"},
		"svc.labels.team": {KindOverlaySkipped, "the schema declares no properties here (an open map)"},
		"svc.extra":       {KindOverlaySkipped, "null in values"},
		"svc.undeclared":  {KindOverlaySkipped, "no schema node for this key"},
	}
	got := map[string]Finding{}
	for _, f := range r.Report.Findings {
		if _, ok := want[f.Path]; ok {
			got[f.Path] = f
		}
	}
	for p, w := range want {
		f, ok := got[p]
		if !ok || f.Kind != w.kind || f.Detail != w.detail {
			t.Errorf("%s: finding %+v, want %s %q", p, f, w.kind, w.detail)
		}
	}
	// Nothing invented: the open map got no properties, the undeclared key no node.
	if get("properties", "svc", "properties", "undeclared") != nil {
		t.Error("an undeclared key got a schema node")
	}
	if get("properties", "svc", "properties", "labels", "properties") != nil {
		t.Error("an open map got properties")
	}
}

// authn's review, minor: with two closed paths that name no map, the error names the same one on
// every run (the alphabetically first), not whichever a map iteration yields.
func TestInfer_UnusedClosedPathErrorIsDeterministic(t *testing.T) {
	fsys := fstest.MapFS{
		"c/Chart.yaml":  {Data: []byte("apiVersion: v2\nname: c\nversion: 0.1.0\nannotations:\n  krateo.io/values-schema-closed: \"zeta, alpha\"\n")},
		"c/values.yaml": {Data: []byte("image: {tag: x}\n")},
	}
	for i := 0; i < 50; i++ {
		_, err := Infer(fsys, "c")
		if err == nil || !strings.Contains(err.Error(), `"alpha"`) {
			t.Fatalf("run %d: err = %v, want it to name \"alpha\"", i, err)
		}
	}
}
