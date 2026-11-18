//go:build envtest

package valuesschema

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"reflect"
	"sort"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// The crdgen round-trip gate for inferred schemas (core-provider#568, child 2), second half: for
// every fixture, the CRD crdgen generates from the inferred schema is ACCEPTED by a real apiserver,
// and a composition whose spec is the chart's COALESCED default values (what helm renders with when
// the user sets nothing) is stored with NOTHING pruned or changed. A pruned value is one the user
// can see in values.yaml, set, and silently lose; a rejected one is a definition that cannot sync.
//
// The coalesced values are computed by helm itself (chartutil.ProcessDependenciesWithMerge, then
// CoalesceValues, as action.Install does), not re-derived here, so the gate measures the inference
// against helm rather than against the inference's own idea of helm.
//
// CONTROL: the `closed` fixture closes `image`; the same coalesced values plus an undeclared
// image.extra must come back with exactly image.extra pruned. Without it, a prunedPaths that never
// reported anything would pass every fixture.

func startRoundTripEnv(t *testing.T) (*rest.Config, func()) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	return cfg, func() { _ = env.Stop() }
}

// coalescedDefaults is what helm renders the chart at dir with when the user supplies no values,
// as JSON-compatible data. Nulls are removed, as helm's coalesce deletes a key whose value is null.
func coalescedDefaults(t *testing.T, dir string) map[string]any {
	t.Helper()
	chrt, err := loader.Load(path.Join("testdata", dir))
	if err != nil {
		t.Fatalf("helm load %s: %v", dir, err)
	}
	vals := map[string]any{}
	if err := chartutil.ProcessDependenciesWithMerge(chrt, vals); err != nil {
		t.Fatalf("helm process dependencies %s: %v", dir, err)
	}
	coalesced, err := chartutil.CoalesceValues(chrt, vals)
	if err != nil {
		t.Fatalf("helm coalesce %s: %v", dir, err)
	}
	out := jsonNormalize(t, map[string]any(coalesced)).(map[string]any)
	dropNulls(out)
	return out
}

// jsonNormalize round-trips v through JSON, so numbers compare as float64 on both sides whatever
// the YAML decoder or the dynamic client produced.
func jsonNormalize(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func dropNulls(m map[string]any) {
	for k, v := range m {
		switch t := v.(type) {
		case nil:
			delete(m, k)
		case map[string]any:
			dropNulls(t)
		}
	}
}

// prunedPaths lists, sorted, every leaf of submitted that stored lacks or holds differently. Maps are
// walked; a list is a leaf, compared whole. Keys only stored has (defaults applied) are not reported.
func prunedPaths(submitted, stored map[string]any, prefix string) []string {
	var out []string
	for k, sv := range submitted {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		gv, ok := stored[k]
		if !ok {
			out = append(out, p)
			continue
		}
		sm, sIsMap := sv.(map[string]any)
		gm, gIsMap := gv.(map[string]any)
		if sIsMap && gIsMap {
			out = append(out, prunedPaths(sm, gm, p)...)
			continue
		}
		if !reflect.DeepEqual(sv, gv) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// storeSpec creates the CRD crdgen generates for schema, then a composition with spec, and returns
// the spec the apiserver stored.
func storeSpec(t *testing.T, cfg *rest.Config, kind string, schemaBytes []byte, spec map[string]any) map[string]any {
	t.Helper()
	ctx := context.Background()
	out, _ := generate(t, kind, schemaBytes)
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	ext := apiextclient.NewForConfigOrDie(cfg)
	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
		t.Fatalf("the apiserver refused the generated CRD: %v", err)
	}
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: crd.Spec.Names.Plural}
	res := dynamic.NewForConfigOrDie(cfg).Resource(gvr).Namespace("default")
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v1", "kind": kind,
		"metadata": map[string]any{"name": "defaults", "namespace": "default"},
		"spec":     spec,
	}}
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err = res.Create(ctx, obj, metav1.CreateOptions{})
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond) // the CRD is not served yet
	}
	if err != nil {
		t.Fatalf("the apiserver refused the chart's own coalesced defaults: %v", err)
	}
	got, err := res.Get(ctx, "defaults", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := jsonNormalize(t, got.Object["spec"]).(map[string]any)
	return stored
}

func TestEnvtest_RoundTrip_CoalescedDefaultsAreStoredUnpruned(t *testing.T) {
	cfg, stop := startRoundTripEnv(t)
	defer stop()
	for _, name := range sortedFixtures() {
		t.Run(name, func(t *testing.T) {
			r := infer(t, fixtures[name])
			submitted := coalescedDefaults(t, fixtures[name])
			if len(submitted) == 0 {
				t.Fatalf("helm coalesced no values for %s: the comparison below would be vacuous", name)
			}
			stored := storeSpec(t, cfg, crdKind(name), r.Schema, submitted)
			if p := prunedPaths(submitted, stored, ""); len(p) > 0 {
				t.Errorf("the apiserver pruned or changed %d value(s) of the chart's coalesced defaults: %v", len(p), p)
			}
		})
	}
}

func TestEnvtest_RoundTrip_ControlClosedMapPrunesAnUndeclaredKey(t *testing.T) {
	cfg, stop := startRoundTripEnv(t)
	defer stop()
	r := infer(t, fixtures["closed"])
	submitted := coalescedDefaults(t, fixtures["closed"])
	image, ok := submitted["image"].(map[string]any)
	if !ok {
		t.Fatalf("closed fixture has no image map: %v", submitted)
	}
	image["extra"] = "not-declared"
	stored := storeSpec(t, cfg, "ClosedControl", r.Schema, submitted)
	if p := prunedPaths(submitted, stored, ""); !reflect.DeepEqual(p, []string{"image.extra"}) {
		t.Errorf("pruned paths = %v, want exactly [image.extra]: the detector does not measure pruning", p)
	}
}
