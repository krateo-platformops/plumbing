//go:build envtest

package crdgen

import (
	"context"
	"fmt"
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

// plumbing#211 on a real apiserver, where it was measured: the reported shape (serviceAccount.name
// required and non-empty when create is false) admitted {create: false, name: ""}. And a condition
// on a defaulted property under a parent with a materialized {} default: the apiserver must accept
// the CRD, its own default literal included, and default before it validates.
func TestEnvtest_ConditionalConstraintsAreEnforced(t *testing.T) {
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
	  "serviceAccount":{"type":"object",
	    "properties":{"create":{"type":"boolean"},"name":{"type":"string"}},
	    "if":{"properties":{"create":{"const":false}}},
	    "then":{"required":["name"],"properties":{"name":{"minLength":1}}}},
	  "service":{"type":"object",
	    "properties":{"type":{"type":"string","enum":["ClusterIP","NodePort"],"default":"ClusterIP"},
	      "nodePort":{"type":"integer","minimum":30000,"maximum":32767}},
	    "if":{"properties":{"type":{"const":"NodePort"}}},
	    "then":{"required":["nodePort"]}}}}`
	out, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(spec)})
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
		t.Fatalf("the apiserver refused the CRD: %v", err)
	}
	res := dyn.Resource(schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: crd.Spec.Names.Plural}).Namespace("default")
	n := 0
	create := func(spec map[string]any) error {
		n++
		_, err := res.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/v1", "kind": "Probe",
			"metadata": map[string]any{"name": fmt.Sprintf("p%d", n), "namespace": "default"},
			"spec":     spec,
		}}, metav1.CreateOptions{})
		return err
	}
	sa := func(v map[string]any) map[string]any { return map[string]any{"serviceAccount": v} }

	// Wait for the CRD to serve with an object it admits, which is also the first admitted case.
	if err := eventually(func() error { return create(sa(map[string]any{"create": false, "name": "sa"})) }); err != nil {
		t.Fatalf("create false with a name was refused: %v", err)
	}
	for _, ok := range []map[string]any{
		sa(map[string]any{"create": true, "name": ""}),
		{"service": map[string]any{}},
		{},
		{"service": map[string]any{"type": "NodePort", "nodePort": 30001}},
	} {
		if err := create(ok); err != nil {
			t.Errorf("refused %v: %v", ok, err)
		}
	}
	for _, bad := range []map[string]any{
		sa(map[string]any{"create": false, "name": ""}),
		sa(map[string]any{"create": false}),
		{"service": map[string]any{"type": "NodePort"}},
	} {
		if err := create(bad); err == nil {
			t.Errorf("admitted %v, which the schema refuses", bad)
		}
	}
}
