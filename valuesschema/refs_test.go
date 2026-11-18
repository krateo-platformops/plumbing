package valuesschema

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/crdgen"
)

// authn's review of plumbing#215, (1): a subchart's own schema is embedded verbatim, so its LOCAL
// refs must move with it. Before the rebase, "#/definitions/endpoint" inside properties.ports
// resolved against the PARENT's root, which has no definitions, and crdgen turned every such node
// into an open object with an "unresolved $ref" warning: the authored types were lost.
func TestRebaseLocalRefs(t *testing.T) {
	in := `{"a":{"$ref":"#/definitions/x"},"b":{"$ref":"#/$defs/y"},"self":{"$ref":"#"},"anchor":{"$ref":"#name"},` +
		`"remote":{"$ref":"other.json#/x"},"default":"#/definitions/x","enum":["#/definitions/x"],"n":1.50,"z":null}`
	got, err := rebaseLocalRefs([]byte(in), "#/properties/sub")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"$ref":"#/properties/sub/definitions/x"},"b":{"$ref":"#/properties/sub/$defs/y"},"self":{"$ref":"#/properties/sub"},"anchor":{"$ref":"#name"},` +
		`"remote":{"$ref":"other.json#/x"},"default":"#/definitions/x","enum":["#/definitions/x"],"n":1.50,"z":null}`
	if string(got) != want {
		t.Fatalf("rebaseLocalRefs:\n got  %s\n want %s", got, want)
	}
	if _, err := rebaseLocalRefs([]byte(`{"a":1} {"b":2}`), "#/x"); err == nil {
		t.Error("trailing data accepted")
	}
}

func TestInfer_RebasesSubchartLocalRefs(t *testing.T) {
	r := infer(t, "refs")
	s := string(r.Schema)
	for _, want := range []string{
		`"$ref":"#/properties/ports/$defs/endpoint"`,              // admin: no value under it, so not inlined
		`"$ref":"#/properties/ports/definitions/port"`,            // a ref inside a $def, to a sibling
		`"$ref":"#/properties/wrapper/properties/tls/$defs/mode"`, // two levels down
		`"default":"#/definitions/endpoint"`,                      // a look-alike string is not a ref
	} {
		if !strings.Contains(s, want) {
			t.Errorf("inferred schema lacks %s", want)
		}
	}

	// http: values.yaml sets http.port, so the overlay (overlay.go) inlined http's rebased ref at this
	// use site and defaulted port there; the shared definition keeps its ref (asserted above).
	inlined, port := false, ""
	for _, f := range r.Report.Findings {
		inlined = inlined || (f.Path == "ports.http" && f.Kind == KindRefInlined && f.Detail == "#/properties/ports/definitions/endpoint")
	}
	if !inlined {
		t.Errorf("no RefInlined finding for ports.http: %+v", r.Report.Findings)
	}
	var doc map[string]any
	if err := json.Unmarshal(r.Schema, &doc); err != nil {
		t.Fatal(err)
	}
	if p, ok := dig(doc, "properties", "ports", "properties", "http", "properties", "port", "default"); ok {
		port = fmt.Sprint(p)
	}
	if port != "80" {
		t.Errorf("ports.http.port default = %q, want 80 (from the subchart's values.yaml)", port)
	}

	unresolved := func(schema []byte) []crdgen.Warning {
		t.Helper()
		_, warns, err := crdgen.GenerateWithDiagnostics(crdgen.Options{Group: "composition.krateo.io", Version: "v1", Kind: "Refs", SpecSchema: schema})
		if err != nil {
			t.Fatalf("crdgen: %v", err)
		}
		var out []crdgen.Warning
		for _, w := range warns {
			if strings.Contains(w.Message, "unresolved $ref") {
				out = append(out, w)
			}
		}
		return out
	}
	if w := unresolved(r.Schema); len(w) != 0 {
		t.Errorf("crdgen could not resolve the rebased refs: %+v", w)
	}

	// CONTROL: the same subchart schema embedded WITHOUT the rebase must produce the warning, or
	// the zero above measures a crdgen that never reports one.
	raw, err := os.ReadFile("testdata/refs/charts/ports/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	verbatim, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]json.RawMessage{"ports": raw}})
	if w := unresolved(verbatim); len(w) == 0 {
		t.Fatal("control: an unrebased subchart schema produced no unresolved-$ref warning, so the check above cannot fail")
	}
}

// authn's review, minor: `global` is open at the top of EVERY chart's values, not only the root's,
// as the rules table says.
func TestInfer_GlobalIsOpenAtEveryChartLevel(t *testing.T) {
	r := infer(t, "deps")
	var doc struct {
		Properties map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(r.Schema, &doc); err != nil {
		t.Fatal(err)
	}
	if got := string(doc.Properties["cache"].Properties["global"]); got != `{"type":"object"}` {
		t.Errorf("cache.global = %s, want {\"type\":\"object\"}", got)
	}
	found := false
	for _, f := range r.Report.Findings {
		found = found || (f.Path == "cache.global" && f.Kind == KindGlobalOpen)
	}
	if !found {
		t.Errorf("no GlobalOpen finding at cache.global: %+v", r.Report.Findings)
	}
}

// dig walks nested JSON objects.
func dig(v any, keys ...string) (any, bool) {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}
