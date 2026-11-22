//go:build envtest

package crdgen

import (
	"context"
	"os"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// plumbing#227 on a real apiserver: `const: null` on a typed, non-nullable field is enforced as
// enum [null]. The field may be omitted or null (not stored), and every value is refused.
//
// Control: the CRD crdgen v1.105.0 actually emitted for the same schema
// (testdata/227-const-null-v1.105.0.yaml, generated once from that tag) admits any string.
func TestEnvtest_ConstNullIsEnforced(t *testing.T) {
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

	// install installs the CRD once and returns a function that creates an object of its kind,
	// retrying only while the new resource isn't served yet, so a validation refusal returns at once.
	install := func(out []byte) func(name string, spec map[string]any) (*unstructured.Unstructured, error) {
		t.Helper()
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(out, &crd); err != nil {
			t.Fatal(err)
		}
		if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
			t.Fatalf("the apiserver refused the %s CRD: %v", crd.Spec.Names.Kind, err)
		}
		gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: crd.Spec.Names.Plural}
		return func(name string, spec map[string]any) (*unstructured.Unstructured, error) {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "composition.krateo.io/v1", "kind": crd.Spec.Names.Kind,
				"metadata": map[string]any{"name": name, "namespace": "default"},
				"spec":     spec,
			}}
			var got *unstructured.Unstructured
			var err error
			for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
				got, err = dyn.Resource(gvr).Namespace("default").Create(ctx, obj, metav1.CreateOptions{})
				if err == nil || !apierrors.IsNotFound(err) {
					break
				}
			}
			return got, err
		}
	}

	out, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Fixed227", SpecSchema: []byte(constNullSpec)})
	if err != nil {
		t.Fatal(err)
	}
	create := install(out)
	if got, err := create("omits", map[string]any{}); err != nil {
		t.Errorf("omitting mode was refused: %v", err)
	} else if spec, _ := got.Object["spec"].(map[string]any); spec["count"] != int64(3) {
		t.Errorf("control: count = %#v, want 3 (the CRD still defaults)", spec["count"])
	}
	if got, err := create("null", map[string]any{"mode": nil}); err != nil {
		t.Errorf("mode: null was refused: %v", err)
	} else if spec, _ := got.Object["spec"].(map[string]any); spec["mode"] != nil {
		t.Errorf("mode: null stored as %#v", spec["mode"])
	}
	if _, err := create("value", map[string]any{"mode": "a"}); err == nil || !apierrors.IsInvalid(err) {
		t.Errorf("mode: \"a\" = %v, want refused as invalid (the schema allows only null)", err)
	}

	// Control: v1.105.0's own output admits the value, so the refusal above can fail.
	golden, err := os.ReadFile("testdata/227-const-null-v1.105.0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install(golden)("value", map[string]any{"mode": "a"}); err != nil {
		t.Errorf("control: v1.105.0's CRD refused mode: \"a\" (%v); the test can't tell the fix apart", err)
	}
}

// constNullSpec is plumbing#227's shape: a typed, non-nullable field with const: null, and a typed
// integer default as the control. testdata/227-const-null-v1.105.0.yaml is crdgen v1.105.0's output
// for it (Kind Before227).
const constNullSpec = `{"type":"object","properties":{
  "mode":{"type":"string","const":null,"description":"must not be set"},
  "count":{"type":"integer","default":3}}}`
