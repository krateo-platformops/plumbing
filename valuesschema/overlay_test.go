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
		"svc.foo":            {KindDefaultOverlaid, ""},
		"svc.port":           {KindDefaultOverlaid, "replaced the authored default"},
		"svc.labels":         {KindDefaultOverlaid, "a required map: the coalesced map, pruned to the schema"},
		"svc.tls":            {KindDefaultOverlaid, "a required map: the coalesced map, pruned to the schema"},
		"svc.tls.mode":       {KindDefaultOverlaid, ""},
		"svc.tls.undeclared": {KindOverlaySkipped, "no schema node for this key"},
		"svc.extra":          {KindOverlaySkipped, "null in values"},
		"svc.undeclared":     {KindOverlaySkipped, "no schema node for this key"},
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
	// A required map's default is the coalesced map PRUNED to the schema: the additionalProperties
	// map keeps its keys, the closed tls drops `undeclared` (the apiserver rejects a default carrying
	// a pruned field).
	if got, _ := json.Marshal(get("properties", "svc", "properties", "labels", "default")); string(got) != `{"team":"core"}` {
		t.Errorf("svc.labels default = %s, want {\"team\":\"core\"}", got)
	}
	if got, _ := json.Marshal(get("properties", "svc", "properties", "tls", "default")); string(got) != `{"mode":"strict"}` {
		t.Errorf("svc.tls default = %s, want {\"mode\":\"strict\"} (undeclared pruned)", got)
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

// plumbing#45's shape: a required map whose coalesced value LACKS a key its own schema requires
// gets no default at all, because the apiserver validates a default literally and would reject the
// CRD. Reported, not set.
func TestInfer_RequiredMapLackingItsRequiredChildGetsNoDefault(t *testing.T) {
	fsys := fstest.MapFS{
		"p/Chart.yaml":                  {Data: []byte("apiVersion: v2\nname: p\nversion: 0.1.0\ndependencies:\n  - name: s\n    version: 0.1.0\n")},
		"p/values.yaml":                 {Data: []byte("x: 1\n")},
		"p/charts/s/Chart.yaml":         {Data: []byte("apiVersion: v2\nname: s\nversion: 0.1.0\n")},
		"p/charts/s/values.yaml":        {Data: []byte("tls:\n  ca: abc\n")},
		"p/charts/s/values.schema.json": {Data: []byte(`{"type":"object","required":["tls"],"properties":{"tls":{"type":"object","required":["mode"],"properties":{"mode":{"type":"string"},"ca":{"type":"string"}}}}}`)},
	}
	r, err := Infer(fsys, "p")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(r.Schema, &doc); err != nil {
		t.Fatal(err)
	}
	if d, ok := dig(doc, "properties", "s", "properties", "tls", "default"); ok {
		t.Errorf("tls got default %v: without its required mode the apiserver rejects the CRD (plumbing#45)", d)
	}
	found := false
	for _, f := range r.Report.Findings {
		found = found || (f.Path == "s.tls" && f.Kind == KindOverlaySkipped && strings.Contains(f.Detail, `"s.tls.mode"`))
	}
	if !found {
		t.Errorf("no OverlaySkipped finding naming s.tls.mode: %+v", r.Report.Findings)
	}
	// The leaf below is still defaulted: only the map-level default is withheld.
	if d, _ := dig(doc, "properties", "s", "properties", "tls", "properties", "ca", "default"); d != "abc" {
		t.Errorf("tls.ca default = %v, want abc", d)
	}
}
