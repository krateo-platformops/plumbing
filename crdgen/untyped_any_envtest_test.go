//go:build envtest

package crdgen

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// plumbing#186, the upgrade question (installer): a CRD generated before the fix, already serving
// objects, is UPDATED to the fixed schema. The apiserver must accept the schema change, the stored
// object must read back intact and stay writable, and a list of strings, refused before, must now
// be admitted. Unit validation proves new writes only; this proves the transition on a real server.
func TestEnvtest_UntypedNodeFixUpgradesAnEstablishedCRD(t *testing.T) {
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

	const spec = `{"type":"object","properties":{"app":{"$ref":"#/$defs/helm-values.app"},"anything":{}},
	  "$defs":{"helm-values.app":{"type":"object","properties":{"targetNamespaces":{"$ref":"#/$defs/helm-values.app.targetNamespaces"}}},
	           "helm-values.app.targetNamespaces":{"type":"array","items":{}}}}`
	out, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(spec)})
	if err != nil {
		t.Fatal(err)
	}
	var fixed apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &fixed); err != nil {
		t.Fatal(err)
	}

	// The pre-fix CRD: the same generated CRD with the two untyped nodes as v1.80.0 emitted them
	// (measured: items {"type":"object","x-kubernetes-preserve-unknown-fields":true}).
	old := fixed.DeepCopy()
	sp := old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	app := sp.Properties["app"]
	tn := app.Properties["targetNamespaces"]
	tn.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: openObject()}
	app.Properties["targetNamespaces"] = tn
	sp.Properties["app"] = app
	sp.Properties["anything"] = *openObject()
	old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = sp
	if reflect.DeepEqual(old.Spec, fixed.Spec) {
		t.Fatal("the pre-fix and fixed CRDs are identical: the transition below would test nothing")
	}

	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, old, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the pre-fix CRD: %v", err)
	}
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: fixed.Spec.Names.Plural}
	res := dyn.Resource(gvr).Namespace("default")
	cr := func(name string, items []any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/v1", "kind": "Probe",
			"metadata":   map[string]any{"name": name, "namespace": "default"},
			"spec":       map[string]any{"app": map[string]any{"targetNamespaces": items}, "anything": map[string]any{"k": "v"}},
		}}
	}
	// Wait for the pre-fix CRD to serve, then store an object it admits.
	stored := cr("stored", []any{map[string]any{"a": "b"}})
	if err := eventually(func() error { _, err := res.Create(ctx, stored, metav1.CreateOptions{}); return err }); err != nil {
		t.Fatalf("store an object under the pre-fix CRD: %v", err)
	}
	// Control: the pre-fix CRD refuses a list of strings (the defect this fixes).
	if _, err := res.Create(ctx, cr("strings-before", []any{"cert-manager"}), metav1.CreateOptions{}); err == nil {
		t.Fatal("the pre-fix CRD admitted a list of strings: the pre-state is not the defect")
	}

	// The transition: update the established CRD to the fixed schema.
	cur, err := ext.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, fixed.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Spec = fixed.Spec
	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("the apiserver refused the schema change on an established CRD: %v", err)
	}

	// A list of strings is admitted once the new schema serves.
	if err := eventually(func() error {
		_, err := res.Create(ctx, cr("strings-after", []any{"cert-manager", "trust"}), metav1.CreateOptions{})
		return err
	}); err != nil {
		t.Fatalf("a list of strings is still refused after the schema change: %v", err)
	}
	// The object stored under the old schema reads back intact and stays writable.
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

func eventually(f func() error) error {
	var err error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if err = f(); err == nil {
			return nil
		}
	}
	return err
}
