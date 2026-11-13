package helm

import (
	"errors"
	"testing"

	"helm.sh/helm/v3/pkg/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

// A spec change that only removes objects, all annotated helm.sh/resource-policy: keep. helm rightly
// leaves them alive, so the merge reports deleted 0, nothing is created and nothing is patched. Before
// keptDeletions that read Changed false, so no revision was ever written (krateo-autopilot on
// krateo-057, 2026-11-08: revision 117 for good, four objects still claimed). frontend's case: a
// render that only removes kept objects must still produce a revision.
//
// Each arm names the live state the decision reads, because that is what separates "correctly left
// alive" from "failed to delete", and only the first may count as a change.
func TestKeptDeletions(t *testing.T) {
	keep := map[string]string{kube.ResourcePolicyAnno: kube.KeepPolicy}
	live := func(ann map[string]string) func(*resource.Info) (runtime.Object, error) {
		return func(info *resource.Info) (runtime.Object, error) {
			u := &unstructured.Unstructured{}
			u.SetAPIVersion("v1")
			u.SetKind(info.Mapping.GroupVersionKind.Kind)
			u.SetName(info.Name)
			u.SetNamespace(info.Namespace)
			u.SetAnnotations(ann)
			return u, nil
		}
	}
	notFound := func(info *resource.Info) (runtime.Object, error) {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, info.Name)
	}
	unreadable := func(*resource.Info) (runtime.Object, error) { return nil, errors.New("connection refused") }

	for _, tc := range []struct {
		name    string
		current []string
		target  []string
		deleted []string
		get     func(*resource.Info) (runtime.Object, error)
		want    int
		because string
	}{
		{
			name: "removal only, all kept: counts", current: []string{"ConfigMap/a", "ConfigMap/b", "ConfigMap/c"},
			target: []string{"ConfigMap/a"}, get: live(keep), want: 2,
			because: "helm skipped both because they are kept; the release must still record that it no longer renders them",
		},
		{
			name: "removed and NOT kept, still alive: does not count", current: []string{"ConfigMap/a", "ConfigMap/b"},
			target: []string{"ConfigMap/a"}, get: live(nil), want: 0,
			because: "the delete failed; a revision would drop it from the release and the delete would never be retried",
		},
		{
			name: "keep with another value: does not count", current: []string{"ConfigMap/a", "ConfigMap/b"},
			target: []string{"ConfigMap/a"}, get: live(map[string]string{kube.ResourcePolicyAnno: "delete"}), want: 0,
			because: "helm skips only the exact value keep",
		},
		{
			name: "removed and actually deleted: not kept", current: []string{"ConfigMap/a", "ConfigMap/b"},
			target: []string{"ConfigMap/a"}, deleted: []string{"ConfigMap/b"}, get: live(keep), want: 0,
			because: "a performed deletion already counts as Deleted; counting it again would double it",
		},
		{
			name: "already gone: not kept", current: []string{"ConfigMap/a", "ConfigMap/b"},
			target: []string{"ConfigMap/a"}, get: notFound, want: 0,
			because: "an object that does not exist is not being kept",
		},
		{
			name: "unreadable: not kept", current: []string{"ConfigMap/a", "ConfigMap/b"},
			target: []string{"ConfigMap/a"}, get: unreadable, want: 0,
			because: "nothing can be said, and the safe answer keeps the object in the release so the next pass retries",
		},
		{
			name: "nothing removed: nothing read", current: []string{"ConfigMap/a"},
			target: []string{"ConfigMap/a"}, get: func(*resource.Info) (runtime.Object, error) {
				t.Fatal("get called although nothing was removed from the render")
				return nil, nil
			}, want: 0,
			because: "a steady-state pass must not read anything extra",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current, target, deleted := resourceListOf(t, tc.current), resourceListOf(t, tc.target), resourceListOf(t, tc.deleted)
			if len(current) != len(tc.current) || len(target) != len(tc.target) || len(deleted) != len(tc.deleted) {
				t.Fatalf("fixture not built")
			}
			got := keptDeletions(current, target, deleted, tc.get, maxChangedObjects)
			if got.total() != tc.want {
				t.Fatalf("kept = %d %v, want %d: %s", got.total(), got.names, tc.want, tc.because)
			}
		})
	}
}

// The verdict over the four counts. The krateo-autopilot arm is the one that was missing: nothing
// created, deleted or patched, four kept.
func TestReleaseChanged(t *testing.T) {
	for _, tc := range []struct {
		created, deleted, patched, kept, stale int
		want                                   bool
	}{
		{0, 0, 0, 0, 0, false}, // steady state: no revision
		{0, 0, 0, 4, 0, true},  // removal-only of kept objects (krateo-autopilot, 2026-11-08)
		{0, 0, 0, 0, 1, true},  // version-only render change (plumbing#155, osh-049a)
		{1, 0, 0, 0, 0, true},
		{0, 1, 0, 0, 0, true},
		{0, 0, 1, 0, 0, true},
	} {
		if got := releaseChanged(tc.created, tc.deleted, tc.patched, tc.kept, tc.stale); got != tc.want {
			t.Errorf("releaseChanged(created %d, deleted %d, patched %d, kept %d, stale %d) = %v, want %v",
				tc.created, tc.deleted, tc.patched, tc.kept, tc.stale, got, tc.want)
		}
	}
}
