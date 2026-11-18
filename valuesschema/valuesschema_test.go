package valuesschema

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

var update = flag.Bool("update", false, "rewrite testdata/golden from the current output")

// fixtures: name -> chart dir under testdata.
var fixtures = map[string]string{
	"basic":         "basic",
	"deps":          "deps",
	"closed":        "closed",
	"kyverno-3.9.2": "kyverno-3.9.2/kyverno",
}

func infer(t *testing.T, dir string) *Result {
	t.Helper()
	r, err := Infer(os.DirFS("testdata"), dir)
	if err != nil {
		t.Fatalf("Infer(%s): %v", dir, err)
	}
	return r
}

// decode parses an inferred schema into a generic tree for assertions.
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	return m
}

// at follows properties down a dotted path.
func at(t *testing.T, s map[string]any, path string) map[string]any {
	t.Helper()
	cur := s
	for _, k := range strings.Split(path, ".") {
		props, _ := cur["properties"].(map[string]any)
		next, ok := props[k].(map[string]any)
		if !ok {
			t.Fatalf("no property %q on the way to %q", k, path)
		}
		cur = next
	}
	return cur
}

// Golden files: the exact bytes, so a rule change shows up as a diff a reviewer reads.
func TestInfer_Golden(t *testing.T) {
	for name, dir := range fixtures {
		t.Run(name, func(t *testing.T) {
			r := infer(t, dir)
			golden := filepath.Join("testdata", "golden", name+".schema.json")
			report, _ := json.MarshalIndent(r.Report, "", "  ")
			goldenReport := filepath.Join("testdata", "golden", name+".report.json")
			if *update {
				_ = os.MkdirAll(filepath.Dir(golden), 0o755)
				if err := os.WriteFile(golden, append(r.Schema, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenReport, append(report, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(append(r.Schema, '\n'), want) {
				t.Errorf("schema differs from %s (run with -update and review the diff)", golden)
			}
			wantReport, err := os.ReadFile(goldenReport)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(append(report, '\n'), wantReport) {
				t.Errorf("report differs from %s", goldenReport)
			}
		})
	}
}

// Deterministic: the same chart gives the same bytes, every time (Go map iteration is randomised,
// so a single map range on the output path would show up here).
func TestInfer_Deterministic(t *testing.T) {
	for name, dir := range fixtures {
		first := infer(t, dir).Schema
		for i := 0; i < 100; i++ {
			if got := infer(t, dir).Schema; !bytes.Equal(got, first) {
				t.Fatalf("%s: run %d differs from run 0", name, i)
			}
		}
	}
}

// Every rule of the table, on the fixture that holds one input state each.
func TestInfer_RulesV1(t *testing.T) {
	s := decode(t, infer(t, "basic").Schema)
	if s["type"] != "object" || s["additionalProperties"] != true {
		t.Errorf("root = %v %v, want an OPEN object", s["type"], s["additionalProperties"])
	}
	if m, _ := s[MarkerKey].(map[string]any); m["source"] != "inferred" || m["rules"] != RulesVersion {
		t.Errorf("marker = %v", s[MarkerKey])
	}
	ios := func(path string, dflt any) {
		t.Helper()
		n := at(t, s, path)
		if n["x-kubernetes-int-or-string"] != true || n["type"] != nil || !reflect.DeepEqual(n["default"], dflt) {
			t.Errorf("%s = %v, want int-or-string with default %v", path, n, dflt)
		}
	}
	ios("replicaCount", float64(1))
	ios("image.tag", "1.27") // quoted: a string, never a number
	ios("port", "8080")
	ios("maxSurge", "25%")
	ios("html", "<none>")
	if n := at(t, s, "image"); n["type"] != "object" || n["additionalProperties"] != true || n["default"] != nil {
		t.Errorf("image = %v, want a typed-OPEN object with no default of its own (crdgen stamps parents)", n)
	}
	if n := at(t, s, "enabled"); n["type"] != "boolean" || n["default"] != true {
		t.Errorf("enabled = %v", n)
	}
	if n := at(t, s, "nodeSelector"); !reflect.DeepEqual(n, map[string]any{"type": "object"}) {
		t.Errorf("nodeSelector ({}) = %v, want free-form {type: object}", n)
	}
	if n := at(t, s, "tolerations"); n["type"] != "array" || n["items"] != nil || !reflect.DeepEqual(n["default"], []any{}) {
		t.Errorf("tolerations ([]) = %v", n)
	}
	if n := at(t, s, "podAnnotations"); len(n) != 0 {
		t.Errorf("podAnnotations (~) = %v, want untyped {}", n)
	}
	if n := at(t, s, "cpuRatio"); n["type"] != nil || n["default"] != 0.5 {
		t.Errorf("cpuRatio (float) = %v, want untyped with its default", n)
	}
	if n := at(t, s, "args"); n["items"].(map[string]any)["x-kubernetes-int-or-string"] != true {
		t.Errorf("args = %v", n)
	}
	if n := at(t, s, "ports"); n["items"].(map[string]any)["x-kubernetes-int-or-string"] != true {
		t.Errorf("ports ([80, \"443\"]) = %v: int and string are one class", n)
	}
	if n := at(t, s, "mixed"); len(n["items"].(map[string]any)) != 0 {
		t.Errorf("mixed ([1, true]) = %v, want untyped items", n)
	}
	env := at(t, s, "env")["items"].(map[string]any)
	props := env["properties"].(map[string]any)
	for _, k := range []string{"name", "value", "valueFrom"} {
		if props[k] == nil {
			t.Errorf("env[] lacks %s: a list of maps merges every element's keys", k)
		}
	}
	if strings.Contains(string(mustJSON(t, env)), `"default"`) {
		t.Errorf("env[] carries defaults %s: one element's values are not every element's default", mustJSON(t, env))
	}
	if n := at(t, s, "global"); !reflect.DeepEqual(n, map[string]any{"type": "object"}) {
		t.Errorf("global = %v, want free-form", n)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Properties come out in values.yaml order (the portal shows authoring order).
func TestInfer_PropertyOrderIsValuesOrder(t *testing.T) {
	out := string(infer(t, "basic").Schema)
	order := []string{"replicaCount", "image", "enabled", "nodeSelector", "tolerations", "podAnnotations",
		"cpuRatio", "port", "maxSurge", "args", "ports", "mixed", "env", "global", "html"}
	last := -1
	for _, k := range order {
		i := strings.Index(out, `"`+k+`":`)
		if i < 0 || i < last {
			t.Fatalf("property %q at %d (previous at %d): not in values.yaml order", k, i, last)
		}
		last = i
	}
}

// Dependencies (D8): merged under the parent's block, keyed by alias, always present, a library
// chart free-form, a subchart's own schema verbatim.
func TestInfer_Dependencies(t *testing.T) {
	r := infer(t, "deps")
	s := decode(t, r.Schema)
	if n := at(t, s, "database.enabled"); n["default"] != true {
		t.Errorf("database.enabled = %v: the parent's true must override db's own false", n)
	}
	if n := at(t, s, "database.auth.username"); n["default"] != "admin" {
		t.Errorf("database.auth.username = %v: db's own key, kept under the merge", n)
	}
	if n := at(t, s, "database.auth.password"); n["default"] != "changeme" {
		t.Errorf("database.auth.password = %v", n)
	}
	if n := at(t, s, "common"); !reflect.DeepEqual(n, map[string]any{"type": "object"}) {
		t.Errorf("common (library) = %v", n)
	}
	if n := at(t, s, "schemaful.mode"); !reflect.DeepEqual(n["enum"], []any{"slow", "fast"}) {
		t.Errorf("schemaful.mode = %v: the subchart's own schema must be used verbatim", n)
	}
	if n := at(t, s, "cache.size"); n["default"] != float64(64) {
		t.Errorf("cache (not mentioned by the parent) = %v: helm adds the key anyway", n)
	}
	for _, want := range []Finding{{Path: "common", Kind: KindLibraryChart, Detail: "common"}, {Path: "schemaful", Kind: KindSubchartSchemaUsed, Detail: "schemaful"}} {
		found := false
		for _, f := range r.Report.Findings {
			found = found || f == want
		}
		if !found {
			t.Errorf("report lacks %+v: %+v", want, r.Report.Findings)
		}
	}
}

// D3: closing by annotation, and its mistakes are errors.
func TestInfer_ClosedByAnnotation(t *testing.T) {
	s := decode(t, infer(t, "closed").Schema)
	for path, want := range map[string]any{"image": false, "resources.limits": false, "resources.requests": true, "resources": true} {
		if got := at(t, s, path)["additionalProperties"]; got != want {
			t.Errorf("%s additionalProperties = %v, want %v", path, got, want)
		}
	}
	mk := func(annotation, values string) fstest.MapFS {
		return fstest.MapFS{
			"c/Chart.yaml":  {Data: []byte("apiVersion: v2\nname: c\nversion: 0.1.0\nannotations:\n  " + ClosedAnnotation + ": " + annotation + "\n")},
			"c/values.yaml": {Data: []byte(values)},
		}
	}
	if _, err := Infer(mk("imgae", "image:\n  tag: x\n"), "c"); err == nil || !strings.Contains(err.Error(), "imgae") {
		t.Errorf("a typo'd path was accepted: %v", err)
	}
	if _, err := Infer(mk("sel", "sel: {}\n"), "c"); err == nil {
		t.Error("closing an empty map was accepted")
	}
	if _, err := Infer(mk("image.tag", "image:\n  tag: x\n"), "c"); err == nil {
		t.Error("closing a scalar was accepted")
	}
}

// The pairing authn asked to keep explicit: crdgen's materializeDefaultedParents (crdgen/
// nesteddefault.go) stamps `default: {}` on a defaulted parent only when that is safe given the
// parent's `required`. Inferred schemas emit NO `required`, so every defaulted parent qualifies and
// a leaf default fires when its whole map is omitted. If a rules version ever emits `required`, this
// fails, and the stamp's coverage has to be re-examined. A subchart's OWN schema (D8) is verbatim and
// may carry `required`: those subtrees are excluded by the report's SubchartSchemaUsed paths.
func TestInfer_NeverEmitsRequired(t *testing.T) {
	for name, dir := range fixtures {
		r := infer(t, dir)
		verbatim := map[string]bool{}
		for _, f := range r.Report.Findings {
			if f.Kind == KindSubchartSchemaUsed {
				verbatim[f.Path] = true
			}
		}
		var walk func(n map[string]any, path string)
		walk = func(n map[string]any, path string) {
			if verbatim[path] {
				return
			}
			if _, ok := n["required"]; ok {
				t.Errorf("%s: inferred node %q carries required", name, path)
			}
			if props, ok := n["properties"].(map[string]any); ok {
				for k, v := range props {
					walk(v.(map[string]any), join(path, k))
				}
			}
			if items, ok := n["items"].(map[string]any); ok {
				walk(items, path+"[]")
			}
		}
		walk(decode(t, r.Schema), "")
	}
}

// A dependency that is declared but not vendored is an error, not a silently missing key.
func TestInfer_UnvendoredDependencyIsAnError(t *testing.T) {
	fsys := fstest.MapFS{
		"c/Chart.yaml":  {Data: []byte("apiVersion: v2\nname: c\nversion: 0.1.0\ndependencies:\n  - name: missing\n    version: 1.0.0\n")},
		"c/values.yaml": {Data: []byte("a: 1\n")},
	}
	if _, err := Infer(fsys, "c"); err == nil || !strings.Contains(err.Error(), "helm dependency build") {
		t.Errorf("err = %v", err)
	}
}
