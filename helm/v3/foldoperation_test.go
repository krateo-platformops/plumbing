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

// core-provider#423: a kept deletion is one decision, not a write. The merge and the real Upgrade
// both report the same kept child (no request sent in either), and the fold keeps ONE. Writes stay
// doubled (the control), and an upgrade's kept verdict for a child the merge did not report kept is
// folded, so a decision only the upgrade made is not lost.
func TestFoldOperation_AKeptChildIsOneDecision(t *testing.T) {
	v := func(name string, c helmconfig.ObjectChange) helmconfig.ObjectVerdict {
		return helmconfig.ObjectVerdict{Version: "v1", Resource: "configmaps", Namespace: "ns", Name: name, Verb: "delete", Change: c}
	}
	r := &helmconfig.ReconcileResult{
		Objects:         []helmconfig.ObjectVerdict{v("kept", helmconfig.ChangeKept), v("written", helmconfig.ChangeSpec)},
		ObjectsComplete: true,
	}
	foldOperation(r, &helmconfig.OperationResult{
		Objects: []helmconfig.ObjectVerdict{
			v("kept", helmconfig.ChangeKept),            // the same decision again: dropped
			v("written", helmconfig.ChangeSpec),         // a second write: kept (#139)
			v("onlyUpgradeKept", helmconfig.ChangeKept), // a decision only the upgrade made: kept
		},
		ObjectsComplete: true,
	})
	var got []string
	for _, o := range r.Objects {
		got = append(got, o.Name+":"+string(o.Change))
	}
	if want := []string{"kept:kept", "onlyUpgradeKept:kept", "written:specChanged", "written:specChanged"}; !equalStrings(got, want) {
		t.Errorf("objects = %v, want %v", got, want)
	}
}
