package objectclient

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

var deployGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

func newDeploy(replicas int64, image string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("apps/v1")
	u.SetKind("Deployment")
	u.SetNamespace("ns1")
	u.SetName("ctrl")
	_ = unstructured.SetNestedField(u.Object, replicas, "spec", "replicas")
	_ = unstructured.SetNestedField(u.Object, image, "spec", "template", "spec", "image")
	return u
}

func replicasOf(t *testing.T, u *unstructured.Unstructured) int64 {
	t.Helper()
	r, found, err := unstructured.NestedInt64(u.Object, "spec", "replicas")
	if err != nil || !found {
		t.Fatalf("spec.replicas: found=%v err=%v", found, err)
	}
	return r
}

// keepReplicas is the caller the hook exists for (core-provider#393): another writer owns
// spec.replicas, so a full re-apply must carry the live value, not the rendered one.
func keepReplicas(seen *[]int64) func(current, desired *unstructured.Unstructured) error {
	return func(current, desired *unstructured.Unstructured) error {
		r, _, _ := unstructured.NestedInt64(current.Object, "spec", "replicas")
		*seen = append(*seen, r)
		return unstructured.SetNestedField(desired.Object, r, "spec", "replicas")
	}
}

func TestApplyFromCurrentKeepsTheLiveValueAndAppliesTheRest(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if err := Apply(ctx, dyn, deployGVR, newDeploy(0, "v1"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	var seen []int64
	desired := newDeploy(1, "v2")
	if err := Apply(ctx, dyn, deployGVR, desired, ApplyOptions{FromCurrent: keepReplicas(&seen)}); err != nil {
		t.Fatal(err)
	}
	got, err := Get(ctx, dyn, deployGVR, "ns1", "ctrl")
	if err != nil {
		t.Fatal(err)
	}
	if r := replicasOf(t, got); r != 0 {
		t.Errorf("replicas = %d, want the live 0 kept", r)
	}
	if img, _, _ := unstructured.NestedString(got.Object, "spec", "template", "spec", "image"); img != "v2" {
		t.Errorf("image = %q, want the rest of the desired object (v2) applied", img)
	}
	if len(seen) != 1 {
		t.Errorf("FromCurrent called %d times, want 1", len(seen))
	}
}

// The property the hook is for: a concurrent write between the Get and the Update makes the
// Update fail with 409, and the retry must copy from the NEW object, not from the one first read.
func TestApplyFromCurrentRecopiesAfterAConflict(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if err := Apply(ctx, dyn, deployGVR, newDeploy(1, "v1"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	updates := 0
	dyn.PrependReactor("update", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates > 1 {
			return false, nil, nil
		}
		// A concurrent writer (the scale-to-zero helper) wakes the controller to 3 meanwhile.
		live, err := dyn.Tracker().Get(deployGVR, "ns1", "ctrl")
		if err != nil {
			t.Fatal(err)
		}
		u := live.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, int64(3), "spec", "replicas")
		if err := dyn.Tracker().Update(deployGVR, u, "ns1"); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(deployGVR.GroupResource(), "ctrl", errors.New("changed meanwhile"))
	})
	var seen []int64
	if err := Apply(ctx, dyn, deployGVR, newDeploy(0, "v2"), ApplyOptions{FromCurrent: keepReplicas(&seen)}); err != nil {
		t.Fatal(err)
	}
	got, _ := Get(ctx, dyn, deployGVR, "ns1", "ctrl")
	if r := replicasOf(t, got); r != 3 {
		t.Errorf("replicas = %d, want 3: the retry must copy from the object it replaces, not from the first read", r)
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 3 {
		t.Errorf("FromCurrent saw %v, want [1 3] (one call per attempt, each with that attempt's object)", seen)
	}
}

func TestApplyFromCurrentIsNotCalledOnCreate(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	var seen []int64
	if err := Apply(ctx, dyn, deployGVR, newDeploy(2, "v1"), ApplyOptions{FromCurrent: keepReplicas(&seen)}); err != nil {
		t.Fatal(err)
	}
	got, _ := Get(ctx, dyn, deployGVR, "ns1", "ctrl")
	if r := replicasOf(t, got); r != 2 {
		t.Errorf("replicas = %d, want the desired 2 on create", r)
	}
	if len(seen) != 0 {
		t.Errorf("FromCurrent called %d times on create, want 0", len(seen))
	}
}

func TestApplyFromCurrentErrorIsNotRetried(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if err := Apply(ctx, dyn, deployGVR, newDeploy(1, "v1"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	verdict := errors.New("refusing: live object is not ours")
	calls := 0
	err := Apply(ctx, dyn, deployGVR, newDeploy(1, "v2"), ApplyOptions{FromCurrent: func(_, _ *unstructured.Unstructured) error {
		calls++
		return verdict
	}})
	if !errors.Is(err, verdict) {
		t.Errorf("err = %v, want the hook's verdict", err)
	}
	if calls != 1 {
		t.Errorf("hook called %d times, want 1: a verdict is not a transient failure", calls)
	}
	got, _ := Get(ctx, dyn, deployGVR, "ns1", "ctrl")
	if img, _, _ := unstructured.NestedString(got.Object, "spec", "template", "spec", "image"); img != "v1" {
		t.Errorf("image = %q, want v1: a refused apply must write nothing", img)
	}
}

// Control: without the hook the desired value is written, exactly as before.
func TestApplyWithoutFromCurrentWritesTheDesiredValue(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if err := Apply(ctx, dyn, deployGVR, newDeploy(0, "v1"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, dyn, deployGVR, newDeploy(1, "v2"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := Get(ctx, dyn, deployGVR, "ns1", "ctrl")
	if r := replicasOf(t, got); r != 1 {
		t.Errorf("replicas = %d, want the desired 1 without the hook", r)
	}
}
