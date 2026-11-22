//go:build envtest

package crdgen

import (
	"context"
	"os"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// core-provider#613 on a real apiserver: a default kept on an untyped
// (preserve-unknown) node must be ACCEPTED in the CRD and APPLIED to an
// object that omits the field, not just emitted. Also nested, the shape a
// values schema infers: valuesschema puts defaults on leaves only, so an
// omitted `tuning` gets `ratio` only if materializeDefaultedParents counts an
// untyped leaf's default.
//
// Control: the CRD crdgen v1.104.0 actually emitted for the same schema
// (testdata/613-untyped-default-v1.104.0.yaml, generated once from that tag,
// not reconstructed by hand) leaves both absent.
func TestEnvtest_UntypedNodeDefaultIsApplied(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	ext := apiextclient.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx := context.Background()

	apply := func(kind string, out []byte) *unstructured.Unstructured {
		t.Helper()
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(out, &crd); err != nil {
			t.Fatal(err)
		}
		if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
			t.Fatalf("the apiserver refused the %s CRD: %v", kind, err)
		}
		gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: crd.Spec.Names.Plural}
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/v1", "kind": kind,
			"metadata": map[string]any{"name": "omits-both", "namespace": "default"},
			"spec":     map[string]any{},
		}}
		var got *unstructured.Unstructured
		if err := eventually(func() error {
			var err error
			got, err = dyn.Resource(gvr).Namespace("default").Create(ctx, obj, metav1.CreateOptions{})
			return err
		}); err != nil {
			t.Fatalf("create a %s: %v", kind, err)
		}
		return got
	}

	out, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Fixed", SpecSchema: []byte(untypedDefaultSpec)})
	if err != nil {
		t.Fatal(err)
	}
	fixed := apply("Fixed", out)
	spec1, _ := fixed.Object["spec"].(map[string]any)
	if spec1["ratio"] != 0.5 {
		t.Errorf("the apiserver didn't apply the untyped default: spec.ratio = %#v, want 0.5", spec1["ratio"])
	}
	tuning, _ := spec1["tuning"].(map[string]any)
	if tuning["ratio"] != 0.5 {
		t.Errorf("nested: spec.tuning = %#v, want ratio 0.5 (the parent materialized, the leaf defaulted)", spec1["tuning"])
	}
	if spec1["count"] != int64(3) {
		t.Errorf("control: spec.count = %#v, want 3", spec1["count"])
	}

	// Control: v1.104.0's own output for the same schema leaves both absent,
	// so the assertions above can fail.
	golden, err := os.ReadFile("testdata/613-untyped-default-v1.104.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	before := apply("Before", golden)
	spec0, _ := before.Object["spec"].(map[string]any)
	if _, ok := spec0["ratio"]; ok {
		t.Errorf("control: v1.104.0's CRD set spec.ratio = %#v; the test can't tell the fix apart", spec0["ratio"])
	}
	if t0, ok := spec0["tuning"].(map[string]any); ok && t0["ratio"] != nil {
		t.Errorf("control: v1.104.0's CRD set spec.tuning.ratio = %#v", t0["ratio"])
	}
	if spec0["count"] != int64(3) {
		t.Errorf("control: v1.104.0's CRD should still default count: %#v", spec0["count"])
	}
}

// untypedDefaultSpec is core-provider#613's shape: a top-level and a nested
// untyped decimal leaf with a default, and a typed integer default as the
// control. testdata/613-untyped-default-v1.104.0.yaml is crdgen v1.104.0's
// output for it (Kind Before).
const untypedDefaultSpec = `{"type":"object","properties":{
  "ratio":{"default":0.5,"description":"the share"},
  "tuning":{"type":"object","properties":{"ratio":{"default":0.5}}},
  "count":{"type":"integer","default":3}}}`
