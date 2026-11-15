//go:build envtest

package crdgen

import (
	"context"
	"os"
	"reflect"
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

// plumbing#186, the union and allOf arms, on a real apiserver: a CRD generated before the fix
// (both nodes `type: object`), already holding an object at each, is UPDATED to the fixed schema.
// The apiserver must accept the change, the stored object must read back intact and stay writable
// (any value admits the objects the old schema admitted), and a string, refused before, is admitted.
func TestEnvtest_UnionAndAllOfFixUpgradesAnEstablishedCRD(t *testing.T) {
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

	const spec = `{"type":"object","properties":{
	  "u":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]},
	  "s":{"allOf":[{"type":"string"},{"minLength":1}]}}}`
	out, _, err := GenerateWithDiagnostics(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(spec)})
	if err != nil {
		t.Fatal(err)
	}
	var fixed apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &fixed); err != nil {
		t.Fatal(err)
	}

	// The pre-fix CRD: both nodes as v1.81.0 emits them, an open object.
	old := fixed.DeepCopy()
	sp := old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	sp.Properties["u"] = *openObject()
	sp.Properties["s"] = *openObject()
	old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = sp
	if reflect.DeepEqual(old.Spec, fixed.Spec) {
		t.Fatal("the pre-fix and fixed CRDs are identical: the transition below would test nothing")
	}

	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, old, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the pre-fix CRD: %v", err)
	}
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: fixed.Spec.Names.Plural}
	res := dyn.Resource(gvr).Namespace("default")
	cr := func(name string, u, s any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/v1", "kind": "Probe",
			"metadata": map[string]any{"name": name, "namespace": "default"},
			"spec":     map[string]any{"u": u, "s": s},
		}}
	}
	stored := cr("stored", map[string]any{"a": "b"}, map[string]any{"c": "d"})
	if err := eventually(func() error { _, err := res.Create(ctx, stored, metav1.CreateOptions{}); return err }); err != nil {
		t.Fatalf("store an object under the pre-fix CRD: %v", err)
	}
	// Control: the pre-fix CRD refuses the strings and the list (the defect).
	if _, err := res.Create(ctx, cr("strings-before", []any{"a"}, "abc"), metav1.CreateOptions{}); err == nil {
		t.Fatal("the pre-fix CRD admitted a list and a string: the pre-state is not the defect")
	}

	cur, err := ext.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, fixed.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Spec = fixed.Spec
	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("the apiserver refused the schema change on an established CRD: %v", err)
	}

	if err := eventually(func() error {
		_, err := res.Create(ctx, cr("strings-after", []any{"a", "b"}, "abc"), metav1.CreateOptions{})
		return err
	}); err != nil {
		t.Fatalf("a list and a string are still refused after the schema change: %v", err)
	}
	if _, err := res.Create(ctx, cr("string-union", "x", "abc"), metav1.CreateOptions{}); err != nil {
		t.Errorf("the union's string member is refused: %v", err)
	}
	got, err := res.Get(ctx, "stored", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Object["spec"], stored.Object["spec"]) {
		t.Errorf("the stored object changed across the schema change:\n got  %v\n want %v", got.Object["spec"], stored.Object["spec"])
	}
	got.SetLabels(map[string]string{"touched": "true"})
	if _, err := res.Update(ctx, got, metav1.UpdateOptions{}); err != nil {
		t.Errorf("the object stored under the old schema is no longer writable: %v", err)
	}
}
