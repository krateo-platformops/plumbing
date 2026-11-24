//go:build envtest

package crdgen

import (
	"context"
	"os"
	"strings"
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

// A chart's transition rules, through Generate, on a real apiserver: an update that breaks one is
// refused, one that keeps them all is accepted (the control in every row), and the rule with
// optionalOldSelf runs on create too, where a plain oldSelf rule doesn't.
func TestEnvtest_OldSelfRulesAreEnforced(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	ctx := context.Background()

	out, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "OldSelf", SpecSchema: []byte(oldSelfSpec)})
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	if _, err := apiextclient.NewForConfigOrDie(cfg).ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
		t.Fatalf("the apiserver refused the CRD: %v", err)
	}
	client := dynamic.NewForConfigOrDie(cfg).Resource(schema.GroupVersionResource{
		Group: "composition.krateo.io", Version: "v1", Resource: crd.Spec.Names.Plural}).Namespace("default")

	// create retries only while the new resource isn't served yet, so a refusal returns at once.
	create := func(name string, spec map[string]any) (*unstructured.Unstructured, error) {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/v1", "kind": "OldSelf",
			"metadata": map[string]any{"name": name, "namespace": "default"}, "spec": spec,
		}}
		var got *unstructured.Unstructured
		var err error
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			got, err = client.Create(ctx, obj, metav1.CreateOptions{})
			if err == nil || !apierrors.IsNotFound(err) {
				break
			}
		}
		return got, err
	}
	base := func() map[string]any {
		return map[string]any{
			"region": "a", "full": int64(0), "off": int64(5),
			"parent": map[string]any{"child": "x"},
			"byName": map[string]any{"m": map[string]any{"size": int64(5)}},
			"items":  []any{map[string]any{"name": "i", "size": int64(5)}},
		}
	}
	refused := func(t *testing.T, err error, field, message string) {
		t.Helper()
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), message) {
			t.Errorf("err = %v, want Invalid at %s with %q", err, field, message)
		}
	}

	// On create, only optionalOldSelf's rule runs. Both halves: it refuses, and the plain rules don't.
	s := base()
	s["full"] = int64(7)
	_, err = create("starts-at-7", s)
	refused(t, err, "spec.full", "full starts at 0, not above it")
	if !strings.Contains(strings.ToLower(err.Error()), "forbidden") {
		t.Errorf("err = %v, want the rule's reason (Forbidden)", err)
	}

	for name, c := range map[string]struct {
		breakIt        func(spec map[string]any)
		field, message string
	}{
		"region-changed": {func(s map[string]any) { s["region"] = "b" }, "spec.region", "region can't change"},
		"region-removed": {func(s map[string]any) { delete(s, "region") }, "spec", "region can't be removed"},
		"off-decreased":  {func(s map[string]any) { s["off"] = int64(4) }, "spec.off", "self >= oldSelf"},
		"child-removed":  {func(s map[string]any) { s["parent"] = map[string]any{} }, "spec.parent.child", "has(self.child)"},
		"map-value":      {func(s map[string]any) { s["byName"] = map[string]any{"m": map[string]any{"size": int64(4)}} }, "spec.byName[m].size", "size can't shrink"},
		"list-item":      {func(s map[string]any) { s["items"] = []any{map[string]any{"name": "i", "size": int64(4)}} }, "spec.items[0].size", "item size can't shrink"},
	} {
		t.Run(name, func(t *testing.T) {
			obj, err := create(name, base())
			if err != nil {
				t.Fatalf("control: create: %v", err)
			}
			// Control: an update the rules allow, full's included (it had to start at 0).
			spec := base()
			spec["full"], spec["off"] = int64(7), int64(6)
			obj.Object["spec"] = spec
			obj, err = client.Update(ctx, obj, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("control: an allowed update: %v", err)
			}
			broken := base()
			broken["full"], broken["off"] = int64(7), int64(6)
			c.breakIt(broken)
			obj.Object["spec"] = broken
			_, err = client.Update(ctx, obj, metav1.UpdateOptions{})
			refused(t, err, c.field, c.message)
		})
	}
}
