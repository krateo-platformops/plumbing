package helm

import (
	"testing"

	"helm.sh/helm/v3/pkg/action"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// plumbing#74. A ListConfig with no StateMask must inherit helm's own default, NOT zero.
//
// applyListConfig assigned cfg.StateMask through unconditionally, so a caller that simply did not
// set the field replaced action.NewList's ListDeployed|ListFailed with 0 — and helm's
// filterStateMask skips EVERY release when the mask is 0:
//
//	mask := l.StateMask & currentStatus
//	if mask == 0 { continue }
//
// The result was an EMPTY SLICE AND A NIL ERROR. There is nothing a caller can check against that,
// and the plausible value is the destructive one: on an adoption path "no releases exist" means
// install a second release beside the live one (core-provider#303); on a teardown path it means
// "nothing to clean up".
//
// Nobody was broken, because both known callers set the field explicitly — which is also why no
// test covered it. The coverage gap and the absence of impact had the same cause, so it read as
// safe.
func TestApplyListConfig_ZeroStateMaskKeepsHelmsDefault(t *testing.T) {
	// The default helm itself installs, asserted rather than restated so this test fails if helm
	// changes it rather than silently encoding an old value.
	def := action.NewList(&action.Configuration{}).StateMask
	if def == 0 {
		t.Fatal("action.NewList left StateMask at 0, so this test cannot distinguish 'inherited " +
			"the default' from 'assigned zero' — the control for the whole assertion is gone")
	}
	if def&action.ListFailed == 0 {
		t.Errorf("helm's default mask %d does not include ListFailed. The CLI's `list --help` says "+
			"it lists releases that are deployed OR failed, so either helm changed or this "+
			"test is reading the wrong field", def)
	}

	// UNSET: the default survives.
	client := action.NewList(&action.Configuration{})
	applyListConfig(client, &helmconfig.ListConfig{})
	if client.StateMask != def {
		t.Errorf("StateMask = %d after applying a ListConfig with no StateMask, want helm's "+
			"default %d. Zero means 'not specified'; assigning it through makes helm skip every "+
			"release and return an empty slice with a nil error", client.StateMask, def)
	}
	if client.StateMask == 0 {
		t.Error("StateMask is 0, which selects NOTHING: ListReleases would answer 'no releases " +
			"exist' for every namespace, with no error for a caller to notice")
	}

	// SET: an explicit mask still wins, or the fix has broken every caller that does specify one.
	want := helmconfig.ListDeployed | helmconfig.ListFailed | helmconfig.ListPendingInstall
	client = action.NewList(&action.Configuration{})
	applyListConfig(client, &helmconfig.ListConfig{StateMask: want})
	if client.StateMask != action.ListStates(want) {
		t.Errorf("StateMask = %d with an explicit mask, want %d: the zero-value guard must not "+
			"swallow a caller's own selection", client.StateMask, action.ListStates(want))
	}

	// AND THE EXPLICIT VALUE MUST BE DISTINGUISHABLE FROM THE DEFAULT, or the arm above passes for
	// the wrong reason — it would be satisfied by a guard that never applied anything.
	if action.ListStates(want) == def {
		t.Fatal("the explicit mask equals helm's default, so the set-arm cannot tell 'applied the " +
			"caller's value' from 'left the default alone'")
	}
}
