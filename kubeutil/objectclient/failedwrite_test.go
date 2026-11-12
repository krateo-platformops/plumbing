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

// core-provider#423: a failed write is still a write the apiserver answered, so a caller recording
// writes records it. On error the result names the write the final attempt sent, or none.

var rolesGR = schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "roles"}

func refuse(verb string) clienttesting.ReactionFunc {
	return func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(rolesGR, "r", errors.New(verb+" refused"))
	}
}

func TestApplyFailedCreateNamesTheCreate(t *testing.T) {
	dyn := newFakeClient(t)
	dyn.PrependReactor("create", "roles", refuse("create"))
	r, err := Apply(context.Background(), dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want the apiserver's Forbidden", err)
	}
	if r.Op != ApplyCreated || r.Before != nil || r.After != nil {
		t.Errorf("result = %+v, want the refused create named, nothing persisted", r)
	}
}

func TestApplyFailedUpdateNamesTheUpdateAndWhatItReplaced(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	dyn.PrependReactor("update", "roles", refuse("update"))
	r, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want Forbidden", err)
	}
	if r.Op != ApplyUpdated || r.Before == nil || r.Before.GetName() != "r" || r.After != nil {
		t.Errorf("result = op %q before %v after %v, want the refused update with Before", r.Op, r.Before, r.After)
	}
}

// No write was sent: a failed read, or a FromCurrent refusal. Nothing to record.
func TestApplyWithNoWriteSentNamesNone(t *testing.T) {
	ctx := context.Background()
	t.Run("the read failed", func(t *testing.T) {
		dyn := newFakeClient(t)
		dyn.PrependReactor("get", "roles", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInternalError(errors.New("etcd down"))
		})
		r, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{})
		if err == nil || r.Op != "" {
			t.Errorf("op %q err %v, want no write named and an error", r.Op, err)
		}
	})
	t.Run("FromCurrent refused", func(t *testing.T) {
		dyn := newFakeClient(t)
		if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		r, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{FromCurrent: func(_, _ *unstructured.Unstructured) error {
			return errors.New("not ours")
		}})
		if err == nil || r.Op != "" || r.Before != nil {
			t.Errorf("result %+v err %v, want no write named", r, err)
		}
	})
}

func TestUninstallFailedDeleteNamesTheDelete(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	dyn.PrependReactor("delete", "roles", refuse("delete"))
	r, err := Uninstall(ctx, dyn, roleGVR, "ns1", "r", UninstallOptions{})
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want Forbidden", err)
	}
	if r.Op != UninstallDeleted || r.Before == nil {
		t.Errorf("result = %+v, want the refused delete named with Before", r)
	}
}

func TestUninstallFailedReadNamesNone(t *testing.T) {
	dyn := newFakeClient(t)
	dyn.PrependReactor("get", "roles", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcd down"))
	})
	r, err := Uninstall(context.Background(), dyn, roleGVR, "ns1", "r", UninstallOptions{})
	if err == nil || r.Op != "" || r.Before != nil {
		t.Errorf("result %+v err %v, want no write named and an error", r, err)
	}
}

// The result is the FINAL attempt's: an earlier attempt's refused Create, followed by attempts that
// fail before sending anything, names no write. Otherwise a write the last attempt never sent would
// be recorded as this call's.
func TestApplyResultIsTheFinalAttempts(t *testing.T) {
	dyn := newFakeClient(t)
	gets := 0
	dyn.PrependReactor("get", "roles", func(clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			return true, nil, apierrors.NewNotFound(rolesGR, "r")
		}
		return true, nil, apierrors.NewInternalError(errors.New("etcd down"))
	})
	creates := 0
	dyn.PrependReactor("create", "roles", func(clienttesting.Action) (bool, runtime.Object, error) {
		creates++
		return true, nil, apierrors.NewInternalError(errors.New("timeout"))
	})
	r, err := Apply(context.Background(), dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{})
	if err == nil {
		t.Fatal("want an error")
	}
	if creates != 1 || gets < 2 {
		t.Fatalf("control: creates=%d gets=%d, want one Create then failed reads", creates, gets)
	}
	if r.Op != "" {
		t.Errorf("op %q, want none: the final attempt sent no write", r.Op)
	}
}

// The same for Uninstall: an earlier attempt's refused Delete, then reads that fail, names no write.
func TestUninstallResultIsTheFinalAttempts(t *testing.T) {
	ctx := context.Background()
	dyn := newFakeClient(t)
	if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	gets := 0
	dyn.PrependReactor("get", "roles", func(a clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 1 {
			return false, nil, nil // the real object
		}
		return true, nil, apierrors.NewInternalError(errors.New("etcd down"))
	})
	deletes := 0
	dyn.PrependReactor("delete", "roles", func(clienttesting.Action) (bool, runtime.Object, error) {
		deletes++
		return true, nil, apierrors.NewInternalError(errors.New("timeout"))
	})
	r, err := Uninstall(ctx, dyn, roleGVR, "ns1", "r", UninstallOptions{})
	if err == nil {
		t.Fatal("want an error")
	}
	if deletes != 1 || gets < 2 {
		t.Fatalf("control: deletes=%d gets=%d, want one Delete then failed reads", deletes, gets)
	}
	if r.Op != "" || r.Before != nil {
		t.Errorf("result %+v, want none: the final attempt sent no write", r)
	}
}
