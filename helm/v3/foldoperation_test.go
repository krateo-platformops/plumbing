package helm

import (
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// plumbing#139: Reconcile folds the result of the real Upgrade it runs into its own. Each verdict is
// one write, so a child both phases changed appears twice; completeness is the AND of both phases;
// the counts add.
func TestFoldOperation(t *testing.T) {
	v := func(name string, c helmconfig.ObjectChange) helmconfig.ObjectVerdict {
		return helmconfig.ObjectVerdict{Version: "v1", Resource: "configmaps", Namespace: "ns", Name: name, Verb: "patch", Change: c}
	}
	r := &helmconfig.ReconcileResult{
		Objects:             []helmconfig.ObjectVerdict{v("a", helmconfig.ChangeSpec), v("c", helmconfig.ChangeCreated)},
		ObjectsComplete:     true,
		ObjectsNotAttempted: 1,
	}
	foldOperation(r, &helmconfig.OperationResult{
		Objects:             []helmconfig.ObjectVerdict{v("b", helmconfig.ChangeSpec), v("a", helmconfig.ChangeSpec)},
		ObjectsComplete:     false,
		ObjectsNotAttempted: 2,
		HooksExcluded:       3,
	})
	var got []string
	for _, o := range r.Objects {
		got = append(got, o.Name+":"+string(o.Change))
	}
	if want := []string{"a:specChanged", "a:specChanged", "b:specChanged", "c:created"}; !equalStrings(got, want) {
		t.Errorf("objects = %v, want %v (both writes to a, sorted by identity)", got, want)
	}
	if r.ObjectsComplete {
		t.Error("an incomplete upgrade makes the reconcile's verdicts incomplete")
	}
	if r.ObjectsNotAttempted != 3 || r.HooksExcluded != 3 {
		t.Errorf("notAttempted=%d hooks=%d, want 3 and 3", r.ObjectsNotAttempted, r.HooksExcluded)
	}

	// A nil operation wrote nothing and changes nothing.
	before := len(r.Objects)
	foldOperation(r, nil)
	if len(r.Objects) != before || r.HooksExcluded != 3 {
		t.Error("a nil operation must leave the result unchanged")
	}
}
