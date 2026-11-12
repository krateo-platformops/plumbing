package objectclient

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

var roleGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}

func newFakeClient(t *testing.T) *fake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		roleGVR: "RoleList",
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
}

func newRole(ns, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("rbac.authorization.k8s.io/v1")
	obj.SetKind("Role")
	obj.SetNamespace(ns)
	obj.SetName(name)
	return obj
}

func TestApplyCreatesWhenAbsent(t *testing.T) {
	dyn := newFakeClient(t)
	ctx := context.Background()

	obj := newRole("ns1", "my-role")
	if _, err := Apply(ctx, dyn, roleGVR, obj, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := Get(ctx, dyn, roleGVR, "ns1", "my-role")
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if got.GetName() != "my-role" {
		t.Fatalf("unexpected name %q", got.GetName())
	}
}

func TestApplyUpdatesWhenPresent(t *testing.T) {
	dyn := newFakeClient(t)
	ctx := context.Background()

	obj := newRole("ns1", "my-role")
	if _, err := Apply(ctx, dyn, roleGVR, obj, ApplyOptions{}); err != nil {
		t.Fatalf("initial Apply: %v", err)
	}

	updated := newRole("ns1", "my-role")
	if err := unstructured.SetNestedField(updated.Object, "changed", "metadata", "labels", "marker"); err != nil {
		t.Fatalf("SetNestedField: %v", err)
	}
	if _, err := Apply(ctx, dyn, roleGVR, updated, ApplyOptions{}); err != nil {
		t.Fatalf("second Apply (update path): %v", err)
	}

	got, err := Get(ctx, dyn, roleGVR, "ns1", "my-role")
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.GetLabels()["marker"] != "changed" {
		t.Fatalf("expected update to be applied, got labels %v", got.GetLabels())
	}
}

func TestGetNotFound(t *testing.T) {
	dyn := newFakeClient(t)
	_, err := Get(context.Background(), dyn, roleGVR, "ns1", "does-not-exist")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestUninstallDeletesWhenPresent(t *testing.T) {
	dyn := newFakeClient(t)
	ctx := context.Background()

	obj := newRole("ns1", "my-role")
	if _, err := dyn.Resource(roleGVR).Namespace("ns1").Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding create: %v", err)
	}

	if _, err := Uninstall(ctx, dyn, roleGVR, "ns1", "my-role", UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	_, err := Get(ctx, dyn, roleGVR, "ns1", "my-role")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected object to be gone, got err %v", err)
	}
}

func TestUninstallIsIdempotentWhenAbsent(t *testing.T) {
	dyn := newFakeClient(t)
	if _, err := Uninstall(context.Background(), dyn, roleGVR, "ns1", "never-existed", UninstallOptions{}); err != nil {
		t.Fatalf("expected Uninstall of a missing object to succeed, got %v", err)
	}
}

func TestClusterScopedWhenNamespaceEmpty(t *testing.T) {
	dyn := newFakeClient(t)
	ctx := context.Background()

	obj := newRole("", "cluster-role")
	if _, err := Apply(ctx, dyn, roleGVR, obj, ApplyOptions{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := Get(ctx, dyn, roleGVR, "", "cluster-role")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetName() != "cluster-role" {
		t.Fatalf("unexpected name %q", got.GetName())
	}
}

// TestApplyWritesBackServerResponse is the regression for the core-provider CompositionDefinition
// "never Ready" digest bug: Apply must REPLACE obj in place with the object the apiserver returned
// (server defaults / admission mutations applied), on both the create and update paths — so that an
// apply-then-hash of obj matches a later get-then-hash of the same live object. Before the fix Apply
// discarded the Create/Update response and left obj as the bare pre-apply render, so the two hashes
// could never converge.
func TestApplyWritesBackServerResponse(t *testing.T) {
	ctx := context.Background()

	// A reactor that mimics the apiserver defaulting a field the caller never set.
	inject := func(action clienttesting.Action) (bool, runtime.Object, error) {
		oa, ok := action.(interface{ GetObject() runtime.Object })
		if !ok {
			return false, nil, nil
		}
		out := oa.GetObject().(*unstructured.Unstructured).DeepCopy()
		if err := unstructured.SetNestedField(out.Object, "server-default", "spec", "injectedByServer"); err != nil {
			return true, nil, err
		}
		return true, out, nil
	}

	// CREATE path.
	dyn := newFakeClient(t)
	dyn.PrependReactor("create", "roles", inject)
	obj := newRole("ns1", "my-role")
	if _, err := Apply(ctx, dyn, roleGVR, obj, ApplyOptions{}); err != nil {
		t.Fatalf("Apply (create path): %v", err)
	}
	if v, _, _ := unstructured.NestedString(obj.Object, "spec", "injectedByServer"); v != "server-default" {
		t.Fatalf("create path: obj was not replaced with the server response (spec.injectedByServer=%q)", v)
	}

	// UPDATE path: seed the object with a plain Apply, then apply again through the injecting reactor.
	dyn2 := newFakeClient(t)
	if _, err := Apply(ctx, dyn2, roleGVR, newRole("ns2", "my-role"), ApplyOptions{}); err != nil {
		t.Fatalf("seed Apply: %v", err)
	}
	dyn2.PrependReactor("update", "roles", inject)
	obj2 := newRole("ns2", "my-role")
	if _, err := Apply(ctx, dyn2, roleGVR, obj2, ApplyOptions{}); err != nil {
		t.Fatalf("Apply (update path): %v", err)
	}
	if v, _, _ := unstructured.NestedString(obj2.Object, "spec", "injectedByServer"); v != "server-default" {
		t.Fatalf("update path: obj was not replaced with the server response (spec.injectedByServer=%q)", v)
	}
}
