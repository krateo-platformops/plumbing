package valuesschema

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// Helm coalescing conformance (core-provider#568, child 3). Two questions, each answered by helm
// itself rather than by this package's idea of helm:
//
//  1. Does helm accept the chart with the inferred schema EMBEDDED as its values.schema.json, when
//     the user sets nothing? A real client-only install (what `helm template` runs) coalesces the
//     values, validates them against the schema and renders. CONTROL: the same schema with the root
//     closed and one dependency's key removed must be refused, which proves helm validated against
//     the embedded schema at all.
//
//  2. Does valuesschema's mergeOver (the only place it reimplements helm's coalesce) compute what
//     chartutil.CoalesceValues computes, for every dependency, at every depth? Two things are
//     deliberately NOT compared, because the rules table does not infer them:
//     - `global` at the top of every chart: free-form ({type: object}); helm pushes the parent's
//       globals down and lets them win, so no chart's own global block is a schema.
//     - a library-chart dependency: free-form ({type: object}); helm adds the key anyway.
//     Nulls are dropped on both sides: helm deletes a key whose value is null, and valuesschema
//     emits an untyped node with no default for it, which the CRD treats the same way (absent).

// helmLoad loads a fixture as helm does, with every dependency ENABLED and its alias applied. The
// inference infers every dependency whatever its condition or tags, so the comparison must see
// every one too; clearing conditions and tags and letting ProcessDependencies run applies aliases
// (it copies an aliased chart under its alias) without dropping anything.
func helmLoad(t *testing.T, dir string) *helmchart.Chart {
	t.Helper()
	chrt, err := loader.Load(path.Join("testdata", dir))
	if err != nil {
		t.Fatalf("helm load %s: %v", dir, err)
	}
	var enableAll func(c *helmchart.Chart)
	enableAll = func(c *helmchart.Chart) {
		for _, d := range c.Metadata.Dependencies {
			d.Condition, d.Tags = "", nil
		}
		for _, sub := range c.Dependencies() {
			enableAll(sub)
		}
	}
	enableAll(chrt)
	if err := chartutil.ProcessDependenciesWithMerge(chrt, map[string]any{}); err != nil {
		t.Fatalf("helm process dependencies %s: %v", dir, err)
	}
	return chrt
}

// renderClientOnly runs a real helm install, client-only and dry-run, which is `helm template`.
func renderClientOnly(chrt *helmchart.Chart) error {
	cfg := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...interface{}) {},
	}
	inst := action.NewInstall(cfg)
	inst.ClientOnly, inst.DryRun, inst.ReleaseName, inst.Namespace = true, true, "conformance", "default"
	// helm's client-only default is Kubernetes v1.20.0; kyverno 3.9.2 requires >=1.25. Render for the
	// version the envtest suites run (Makefile ENVTEST_K8S_VERSION), so kubeVersion gates pass.
	inst.KubeVersion = &chartutil.KubeVersion{Version: "v1.36.0", Major: "1", Minor: "36"}
	_, err := inst.RunWithContext(context.Background(), chrt, map[string]any{})
	return err
}

func TestHelmConformance_DefaultValuesPassTheEmbeddedInferredSchema(t *testing.T) {
	for _, name := range sortedFixtures() {
		t.Run(name, func(t *testing.T) {
			r := infer(t, fixtures[name])
			chrt, err := loader.Load(path.Join("testdata", fixtures[name]))
			if err != nil {
				t.Fatal(err)
			}
			chrt.Schema = r.Schema
			if err := renderClientOnly(chrt); err != nil {
				t.Errorf("helm refused the chart's own defaults against its inferred schema: %v", err)
			}
		})
	}
}

// CONTROL for the test above: close the root and remove one dependency's key. helm coalesces the
// dependency's values under that key whatever the schema says, so a closed root without it must be
// refused. If this passed, the test above would prove nothing about the embedded schema.
func TestHelmConformance_ControlClosedRootWithoutADependencyKeyIsRefused(t *testing.T) {
	r := infer(t, fixtures["deps"])
	var doc map[string]any
	if err := json.Unmarshal(r.Schema, &doc); err != nil {
		t.Fatal(err)
	}
	props := doc["properties"].(map[string]any)
	if _, ok := props["database"]; !ok {
		t.Fatalf("deps' inferred root has no `database` key to remove: %v", props)
	}
	delete(props, "database")
	doc["additionalProperties"] = false
	closed, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	chrt, err := loader.Load(path.Join("testdata", fixtures["deps"]))
	if err != nil {
		t.Fatal(err)
	}
	chrt.Schema = closed
	err = renderClientOnly(chrt)
	if err == nil || !strings.Contains(err.Error(), "database") {
		t.Errorf("err = %v, want a schema refusal naming `database`", err)
	}
}

// inferredCoalesce is the values tree valuesschema reasons about: each dependency's own values with
// the parent's block for it merged over (mergeOver), recursively, under its name or alias, exactly as
// chartNode and dependencyNode walk it. Library dependencies are skipped (see the comparison rules).
func inferredCoalesce(c *chart, values *value) *value {
	if values == nil {
		values = &value{kind: kindMap, entries: map[string]*value{}}
	}
	out := &value{kind: kindMap, entries: map[string]*value{}}
	for _, k := range values.keys {
		out.set(k, values.entries[k])
	}
	for _, d := range c.deps {
		sub := c.subcharts[d.name]
		if sub == nil || sub.chartType == "library" {
			continue
		}
		out.set(d.key(), inferredCoalesce(sub, mergeOver(sub.values, values.entries[d.key()])))
	}
	return out
}

// comparable drops, in place, what the comparison rules exclude: nulls, `global` at the top of
// every chart level, and library dependencies' keys. m is the top of chart c's values; each
// dependency's key is descended into as the top of that dependency's values.
func comparable(m map[string]any, c *chart) {
	delete(m, "global")
	depKeys := map[string]*chart{}
	for _, d := range c.deps {
		depKeys[d.key()] = c.subcharts[d.name]
	}
	var dropNullsDeep func(m map[string]any)
	dropNullsDeep = func(m map[string]any) {
		for k, v := range m {
			switch t := v.(type) {
			case nil:
				delete(m, k)
			case map[string]any:
				dropNullsDeep(t)
			}
		}
	}
	for k, v := range m {
		sub, isDep := depKeys[k]
		if !isDep {
			continue
		}
		if sub == nil || sub.chartType == "library" {
			delete(m, k)
			continue
		}
		if sm, ok := v.(map[string]any); ok {
			comparable(sm, sub)
		}
	}
	dropNullsDeep(m)
}

// diffPaths lists, sorted, every path where a and b differ (a list is compared whole).
func diffPaths(a, b map[string]any, prefix string) []string {
	var out []string
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		av, aok := a[k]
		bv, bok := b[k]
		am, aIsMap := av.(map[string]any)
		bm, bIsMap := bv.(map[string]any)
		switch {
		case !aok || !bok:
			out = append(out, p)
		case aIsMap && bIsMap:
			out = append(out, diffPaths(am, bm, p)...)
		case !reflect.DeepEqual(av, bv):
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func toComparable(t *testing.T, v any, c *chart) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	comparable(m, c)
	return m
}

func TestHelmConformance_MergeOverMatchesCoalesceValues(t *testing.T) {
	for _, name := range sortedFixtures() {
		t.Run(name, func(t *testing.T) {
			c, err := loadChart(os.DirFS("testdata"), fixtures[name])
			if err != nil {
				t.Fatal(err)
			}
			ours := toComparable(t, inferredCoalesce(c, c.values).toJSON(), c)
			helmVals, err := chartutil.CoalesceValues(helmLoad(t, fixtures[name]), map[string]any{})
			if err != nil {
				t.Fatalf("helm coalesce: %v", err)
			}
			theirs := toComparable(t, map[string]any(helmVals), c)
			if len(theirs) == 0 {
				t.Fatal("helm coalesced nothing: the comparison would be vacuous")
			}
			if d := diffPaths(ours, theirs, ""); len(d) > 0 {
				t.Errorf("valuesschema's merge and helm's coalesce differ at %d path(s): %v", len(d), d)
			}
		})
	}
}

// CONTROL for the comparison: a mergeOver that let the dependency's own value win over the parent's
// block (the inverted precedence) must be caught on the coalesce fixture, where the parent overrides
// fast.replicas. Without it, a diffPaths that never reported anything would pass every fixture.
func TestHelmConformance_ControlInvertedPrecedenceIsCaught(t *testing.T) {
	c, err := loadChart(os.DirFS("testdata"), fixtures["coalesce"])
	if err != nil {
		t.Fatal(err)
	}
	var inverted func(c *chart, values *value) *value
	inverted = func(c *chart, values *value) *value {
		out := &value{kind: kindMap, entries: map[string]*value{}}
		for _, k := range values.keys {
			out.set(k, values.entries[k])
		}
		for _, d := range c.deps {
			sub := c.subcharts[d.name]
			if sub == nil || sub.chartType == "library" {
				continue
			}
			out.set(d.key(), inverted(sub, mergeOver(values.entries[d.key()], sub.values))) // swapped
		}
		return out
	}
	ours := toComparable(t, inverted(c, c.values).toJSON(), c)
	helmVals, err := chartutil.CoalesceValues(helmLoad(t, fixtures["coalesce"]), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	theirs := toComparable(t, map[string]any(helmVals), c)
	d := diffPaths(ours, theirs, "")
	if !contains(d, "fast.replicas") {
		t.Errorf("diff = %v, want it to include fast.replicas: the comparison does not measure precedence", d)
	}
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
