//go:build envtest

package objectclient

import (
	"context"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var configMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

func configMap(data string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("ConfigMap")
	u.SetNamespace("default")
	u.SetName("apply-result")
	_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"k": data}, "data")
	return u
}

// core-provider#423, against a REAL apiserver: an Update that changes nothing persists nothing (the
// apiserver short-circuits it, resourceVersion unmoved), which Apply reports as unchanged; one that
// changes content is updated. Without the real apiserver this half is unprovable: the fake keeps
// whatever resourceVersion it is sent.
func TestEnvtest_ApplyTellsUnchangedFromUpdated(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	created, err := Apply(ctx, dyn, configMapGVR, configMap("a"), ApplyOptions{})
	if err != nil || created.Op != ApplyCreated {
		t.Fatalf("first apply: op %q err %v, want created", created.Op, err)
	}

	same, err := Apply(ctx, dyn, configMapGVR, configMap("a"), ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if same.Op != ApplyUnchanged {
		t.Errorf("an apply that changes nothing: op %q, want unchanged (resourceVersion %s -> %s)",
			same.Op, same.Before.GetResourceVersion(), same.After.GetResourceVersion())
	}

	changed, err := Apply(ctx, dyn, configMapGVR, configMap("b"), ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Op != ApplyUpdated {
		t.Errorf("an apply that changes data: op %q, want updated", changed.Op)
	}
	if v, _, _ := unstructured.NestedString(changed.Before.Object, "data", "k"); v != "a" {
		t.Errorf("Before must be the replaced version (data.k=a), got %q", v)
	}
	if v, _, _ := unstructured.NestedString(changed.After.Object, "data", "k"); v != "b" {
		t.Errorf("After must be the persisted version (data.k=b), got %q", v)
	}

	// Control: the server really persisted the change, and only once.
	live, err := Get(ctx, dyn, configMapGVR, "default", "apply-result")
	if err != nil {
		t.Fatal(err)
	}
	if live.GetResourceVersion() != changed.After.GetResourceVersion() || live.GetResourceVersion() == same.After.GetResourceVersion() {
		t.Errorf("control: live resourceVersion %s; unchanged left %s, updated wrote %s",
			live.GetResourceVersion(), same.After.GetResourceVersion(), changed.After.GetResourceVersion())
	}

	del, err := Uninstall(ctx, dyn, configMapGVR, "default", "apply-result", UninstallOptions{})
	if err != nil || del.Op != UninstallDeleted || del.Before == nil {
		t.Fatalf("uninstall: %+v err %v, want deleted with Before", del, err)
	}
	gone, err := Uninstall(ctx, dyn, configMapGVR, "default", "apply-result", UninstallOptions{})
	if err != nil || gone.Op != UninstallAbsent {
		t.Fatalf("second uninstall: %+v err %v, want absent", gone, err)
	}
}
