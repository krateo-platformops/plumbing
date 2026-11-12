package objectclient

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

// core-provider#423: Apply and Uninstall report what they did. Whether an Update persisted anything
// is the apiserver's to say (resourceVersion), so that half is pinned against a real one in
// result_envtest_test.go; these pin the shape on the fake.

func TestApplyReportsCreatedWithNoBefore(t *testing.T) {
	r, err := Apply(context.Background(), newFakeClient(t), roleGVR, newRole("ns1", "r"), ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Op != ApplyCreated || r.Before != nil || r.After == nil || r.After.GetName() != "r" {
		t.Errorf("result = %+v, want created, no Before, After = the created object", r)
	}
}

func TestApplyReportsTheReplacedVersionAsBefore(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	first := newRole("ns1", "r")
	first.SetLabels(map[string]string{"v": "1"})
	if _, err := Apply(ctx, dyn, roleGVR, first, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	second := newRole("ns1", "r")
	second.SetLabels(map[string]string{"v": "2"})
	r, err := Apply(ctx, dyn, roleGVR, second, ApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Op == ApplyCreated || r.Before == nil || r.Before.GetLabels()["v"] != "1" || r.After.GetLabels()["v"] != "2" {
		t.Errorf("result = op %q before %v after %v, want an update with Before v=1 and After v=2", r.Op, r.Before.GetLabels(), r.After.GetLabels())
	}
}

// Under DryRun nothing persists, so the resourceVersion says nothing: a dry-run Update is never
// reported unchanged. The fake keeps the resourceVersion as sent, which is the no-op shape.
func TestApplyDryRunUpdateIsNeverUnchanged(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	// Control: the same apply without DryRun is unchanged on the fake (resourceVersion unmoved).
	if r, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil || r.Op != ApplyUnchanged {
		t.Fatalf("control: op %q err %v, want unchanged", r.Op, err)
	}
	r, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{DryRun: []string{"All"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Op != ApplyUpdated {
		t.Errorf("dry-run op = %q, want updated", r.Op)
	}
}

func TestUninstallReportsDeletedWithBefore(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err := Uninstall(ctx, dyn, roleGVR, "ns1", "r", UninstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Op != UninstallDeleted || r.Before == nil || r.Before.GetName() != "r" {
		t.Errorf("result = %+v, want deleted with Before = the object", r)
	}
	again, err := Uninstall(ctx, dyn, roleGVR, "ns1", "r", UninstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Op != UninstallAbsent || again.Before != nil {
		t.Errorf("second uninstall = %+v, want absent", again)
	}
}

// A concurrent removal between the Get and the Delete: the Delete answers NotFound, which is not
// this caller's write.
func TestUninstallConcurrentlyRemovedIsAbsent(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	dyn.PrependReactor("delete", "roles", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "roles"}, "r")
	})
	r, err := Uninstall(ctx, dyn, roleGVR, "ns1", "r", UninstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Op != UninstallAbsent || r.Before != nil {
		t.Errorf("result = %+v, want absent", r)
	}
}
