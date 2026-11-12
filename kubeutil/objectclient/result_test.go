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

// snowplow (2026-11-12): a dry run is validated and not persisted, so it is not a write. Every
// dry-run request reports ApplyDryRun / UninstallDryRun (created, updated, unchanged, refused alike),
// so a caller recording writes excludes it by construction. Each case has its non-dry-run control.
func TestDryRunIsNeverAWrite(t *testing.T) {
	ctx := context.Background()
	dryAll := ApplyOptions{DryRun: []string{"All"}}

	t.Run("create", func(t *testing.T) {
		dyn := newFakeClient(t)
		r, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), dryAll)
		if err != nil || r.Op != ApplyDryRun || r.Before != nil {
			t.Errorf("dry-run create: op %q before %v err %v, want dryRun", r.Op, r.Before, err)
		}
		// Control: the same create without DryRun is created.
		if r, err := Apply(ctx, newFakeClient(t), roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil || r.Op != ApplyCreated {
			t.Errorf("control: op %q err %v, want created", r.Op, err)
		}
	})
	t.Run("update, with or without a change", func(t *testing.T) {
		dyn := newFakeClient(t)
		if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		same, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), dryAll)
		if err != nil || same.Op != ApplyDryRun || same.Before == nil {
			t.Errorf("dry-run update, no change: op %q before %v err %v, want dryRun with Before", same.Op, same.Before, err)
		}
		changed := newRole("ns1", "r")
		changed.SetLabels(map[string]string{"x": "y"})
		ch, err := Apply(ctx, dyn, roleGVR, changed, dryAll)
		if err != nil || ch.Op != ApplyDryRun {
			t.Errorf("dry-run update, a change: op %q err %v, want dryRun", ch.Op, err)
		}
		// Control: without DryRun the unchanged apply is unchanged (the fake keeps the rv).
		if r, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil || r.Op != ApplyUnchanged {
			t.Errorf("control: op %q err %v, want unchanged", r.Op, err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		dyn := newFakeClient(t)
		dyn.PrependReactor("create", "roles", refuse("create"))
		r, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), dryAll)
		if err == nil || r.Op != ApplyDryRun {
			t.Errorf("refused dry run: op %q err %v, want dryRun and the error", r.Op, err)
		}
	})
	t.Run("uninstall", func(t *testing.T) {
		dyn := newFakeClient(t)
		if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		r, err := Uninstall(ctx, dyn, roleGVR, "ns1", "r", UninstallOptions{DryRun: []string{"All"}})
		if err != nil || r.Op != UninstallDryRun || r.Before == nil {
			t.Errorf("dry-run uninstall: %+v err %v, want dryRun with Before", r, err)
		}
		// The fake ignores DryRun and really deleted it; a real apiserver would not. Re-create it for
		// the refused case.
		if _, err := Apply(ctx, dyn, roleGVR, newRole("ns1", "r"), ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		dyn.PrependReactor("delete", "roles", refuse("delete"))
		if r, err := Uninstall(ctx, dyn, roleGVR, "ns1", "r", UninstallOptions{DryRun: []string{"All"}}); err == nil || r.Op != UninstallDryRun {
			t.Errorf("refused dry-run uninstall: %+v err %v, want dryRun and the error", r, err)
		}
	})
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
