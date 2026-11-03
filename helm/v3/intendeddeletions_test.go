package helm

// IntendedDeletions exists because `Deleted: 0` cannot distinguish three states (core-provider#304).
//
//	intended 0, deleted 0   nothing was removed from the render. Correct, and the common case.
//	intended 1, deleted 1   the removal converged.
//	intended 1, deleted 0   THE DEFECT. Either helm's deletion loop never ran — it sits AFTER the
//	                        error switch in pkg/kube/client.go, so ONE unpatchable object skips
//	                        every deletion — or the delete was issued and did not stick.
//
// A caller seeing only `deleted: 0` cannot tell the first from the third, and that is precisely how
// #304 survived: a node removed from a spec left its child alive for sixteen minutes while the log
// printed `created:0 deleted:0 patched:10` — the same line every healthy steady-state reconcile
// prints.
//
// THE RETROACTIVE TRAP, which is the reason intent has to be recorded rather than inferred later:
// the reporter was about to hand over six `deleted:0` observations as corroboration, and none of
// those reconciles was a removal. Six independent confirmations of nothing. Without an `intended`
// count there is nothing in the record that distinguishes them.
//
// This tests the SET OPERATION, not the merge. The identity of what helm will delete is
// current.Difference(target) over the lists the merge receives — helm's own function, so the
// matching is helm's (group+kind+name+namespace, version-independent) and cannot drift from it.
// Driving the whole Reconcile would need a release store, a rendered chart, an action config and a
// fake KubeClient; this package prefers extracted decisions for that reason (filterLiveJobs,
// changedObjectSet, classifyApply).

import (
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/kube"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

func TestIntendedDeletions_IsWhatHelmWouldDelete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current []string // "Kind/name"
		target  []string
		want    []string
		because string
	}{
		{
			name:    "nothing removed: no intent",
			current: []string{"ConfigMap/a", "ConfigMap/b"},
			target:  []string{"ConfigMap/a", "ConfigMap/b"},
			want:    nil,
			because: "a steady-state reconcile must report intent 0, or every healthy pass looks " +
				"like the defect",
		},
		{
			name:    "one child dropped from the render: intent names it",
			current: []string{"ConfigMap/a", "ConfigMap/b"},
			target:  []string{"ConfigMap/a"},
			want:    []string{"v1/ConfigMap/default/b"},
			because: "this is #304's case — the child is in the stored manifest and absent from the " +
				"render, so the merge is ASKED to delete it and the caller must be able to say so " +
				"even if nothing is deleted",
		},
		{
			name:    "several dropped: all named",
			current: []string{"ConfigMap/a", "ConfigMap/b", "ConfigMap/c"},
			target:  []string{"ConfigMap/b"},
			want:    []string{"v1/ConfigMap/default/a", "v1/ConfigMap/default/c"},
			because: "a node removal can drop several children at once",
		},
		{
			name:    "a child ADDED to the render is not an intended deletion",
			current: []string{"ConfigMap/a"},
			target:  []string{"ConfigMap/a", "ConfigMap/b"},
			want:    nil,
			because: "Difference is directional; an addition belongs to Created, and counting it " +
				"here would make every install look like a pending deletion",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := resourceListOf(t, tc.current)
			target := resourceListOf(t, tc.target)

			// CALIBRATION: the fixtures must actually be built, or Difference over two empty lists
			// returns empty and every arm above "passes".
			if len(current) != len(tc.current) || len(target) != len(tc.target) {
				t.Fatalf("fixture not built: current %d/%d, target %d/%d",
					len(current), len(tc.current), len(target), len(tc.target))
			}

			// Calls the PRODUCTION function, not Difference directly. The first version of this
			// test called Difference itself, and a mutant that reversed the direction inside
			// Reconcile compiled and left the test passing — it was validating my reading of helm
			// rather than the code that ships.
			// Asserts the IDENTITY STRING the result actually carries — "groupVersion/Kind/ns/name"
			// from infoKey — rather than reparsing it back into a shorthand. A consumer reads this
			// string; a test that reparses it is asserting its own parser.
			got := intendedDeletions(current, target, maxChangedObjects).names

			if len(got) != len(tc.want) {
				t.Fatalf("intended deletions = %v, want %v: %s", got, tc.want, tc.because)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("intended[%d] = %q, want %q: %s", i, got[i], tc.want[i], tc.because)
				}
			}
		})
	}
}

// resourceListOf builds a minimal kube.ResourceList whose entries differ in Kind/name only, which
// is what Difference matches on.
func resourceListOf(t *testing.T, specs []string) kube.ResourceList {
	t.Helper()
	var out kube.ResourceList
	for _, s := range specs {
		out = append(out, infoFromSpec(t, s))
	}
	return out
}

// infoFromSpec builds a resource.Info from "Kind/name".
//
// Mapping is set deliberately and is NOT optional: helm's isMatchingInfo dereferences
// a.Mapping.GroupVersionKind (pkg/kube/resource.go:84), so an Info without it panics inside
// Difference. The package's existing cmInfo/jobInfo helpers leave Mapping nil — correct for
// filterLiveJobs, which matches on the object's own kind — so they cannot be reused here. Worth
// stating rather than quietly adding a near-duplicate: the two helpers differ for a reason.
func infoFromSpec(t *testing.T, spec string) *resource.Info {
	t.Helper()
	parts := strings.SplitN(spec, "/", 2)
	if len(parts) != 2 {
		t.Fatalf("bad fixture spec %q, want Kind/name", spec)
	}
	kind, name := parts[0], parts[1]

	// BOTH Mapping AND Object, because the two consumers need different fields and a fixture that
	// satisfies one silently breaks the other:
	//
	//	Difference -> isMatchingInfo reads info.Mapping.GroupVersionKind (pkg/kube/resource.go:84)
	//	infoKey    -> reads info.Object.GetObjectKind().GroupVersionKind()  (reconcile.go:504)
	//
	// The first version of this helper set only Mapping. Difference worked; infoKey panicked on a
	// nil Object. The package's existing cmInfo/jobInfo helpers set only Object — correct for
	// filterLiveJobs, which matches on the object's own kind — which is why they cannot be reused
	// here rather than why a near-duplicate was added carelessly.
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind(kind)
	obj.SetName(name)
	obj.SetNamespace("default")

	return &resource.Info{
		Name:      name,
		Namespace: "default",
		Object:    obj,
		Mapping: &apimeta.RESTMapping{
			GroupVersionKind: schema.GroupVersionKind{Version: "v1", Kind: kind},
		},
	}
}
