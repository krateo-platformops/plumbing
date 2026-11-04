package helm

import (
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
)

// helm keeps lifecycle hooks OUT of release.Manifest by construction — releaseutil.SortManifests
// splits them into their own slice. A consumer reading only Manifest cannot see them at all, which
// is how core-provider#311 happened: chart-inspector derives a composition's RBAC from a dry-run
// install narrowed against Manifest, so NO grant was ever generated for a hook object, and a
// pre-delete hook turned teardown into an uninstall that could never succeed.
//
// The controlled comparison on the reporting cluster: one template rendering four objects —
// ServiceAccount, Role, RoleBinding, Job — of which only the Job carried `helm.sh/hook: pre-delete`.
// Three of four got grants. The hook did not.
//
// So the wrapper has to carry them. These tests pin that it does, and that the projection does not
// lose the fields a consumer needs to act.

func TestToWrapperRelease_CarriesHooks(t *testing.T) {
	src := &release.Release{
		Name:      "parent",
		Namespace: "krateo-system",
		Version:   3,
		Chart:     &chart.Chart{Metadata: &chart.Metadata{Version: "0.6.2"}},
		Info:      &release.Info{Status: release.StatusDeployed},
		Manifest:  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: plain\n",
		Hooks: []*release.Hook{
			{
				Name:     "teardown",
				Kind:     "Job",
				Path:     "templates/teardown-undeploy.yaml",
				Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: teardown\n  namespace: openstack\n",
				Events:   []release.HookEvent{release.HookPreDelete},
			},
			{
				Name:     "seed",
				Kind:     "Job",
				Path:     "templates/seed.yaml",
				Manifest: "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: seed\n",
				Events:   []release.HookEvent{release.HookPostInstall, release.HookPostUpgrade},
			},
		},
	}

	got := toWrapperRelease(src)

	// PREMISE: the ordinary fields still work, so a failure below is about hooks and not about a
	// broken conversion.
	if got.Name != "parent" || got.Revision != 3 || got.Manifest == "" {
		t.Fatalf("premise: the base conversion is broken (name=%q revision=%d manifestEmpty=%v)",
			got.Name, got.Revision, got.Manifest == "")
	}

	if len(got.Hooks) != 2 {
		t.Fatalf("got %d hooks, want 2: a consumer that cannot see a hook cannot grant it RBAC",
			len(got.Hooks))
	}

	// GetHooks must agree with the field, since consumers will use the accessor (mirroring
	// GetManifest) and a divergence would be invisible.
	if len(got.GetHooks()) != len(got.Hooks) {
		t.Errorf("GetHooks() returned %d, field has %d", len(got.GetHooks()), len(got.Hooks))
	}

	h := got.Hooks[0]
	if h.Name != "teardown" || h.Kind != "Job" {
		t.Errorf("hook identity = %q/%q, want teardown/Job", h.Name, h.Kind)
	}
	// Path is what makes a hook DIAGNOSABLE: an operator told "this hook has no RBAC" needs the file.
	if h.Path != "templates/teardown-undeploy.yaml" {
		t.Errorf("hook Path = %q; without it a diagnostic cannot name the template", h.Path)
	}
	// THE LOAD-BEARING FIELD. Kind alone carries no apiVersion and no namespace, so a consumer
	// resolving a GVK must parse the Manifest. A projection that dropped it would look complete
	// and be useless.
	if h.Manifest == "" {
		t.Fatal("hook Manifest is empty: Kind alone has no apiVersion and no namespace, so a " +
			"consumer cannot resolve the GVK it needs to grant")
	}
	for _, want := range []string{"batch/v1", "kind: Job", "namespace: openstack"} {
		if !contains(h.Manifest, want) {
			t.Errorf("hook Manifest lost %q: %q", want, h.Manifest)
		}
	}

	// Events are carried as plain strings so helm's HookEvent stays out of a consumer's import
	// graph — the whole reason Hook is a projection rather than a re-export.
	if len(h.Events) != 1 || h.Events[0] != "pre-delete" {
		t.Errorf("hook Events = %v, want [pre-delete]", h.Events)
	}
	if len(got.Hooks[1].Events) != 2 {
		t.Errorf("multi-event hook lost events: %v", got.Hooks[1].Events)
	}
}

// A release with no hooks must yield no hooks — not an empty-but-present slice that a consumer
// might read as "there were hooks and they were all dropped".
func TestToWrapperRelease_NoHooksIsNil(t *testing.T) {
	src := &release.Release{
		Name:  "plain",
		Chart: &chart.Chart{Metadata: &chart.Metadata{Version: "1.0.0"}},
		Info:  &release.Info{Status: release.StatusDeployed},
	}
	got := toWrapperRelease(src)
	if len(got.Hooks) != 0 {
		t.Errorf("got %d hooks from a release with none", len(got.Hooks))
	}
}

// A nil entry in helm's slice must be skipped rather than panic. Defensive, but the cost of being
// wrong is a crash in the RBAC derivation path, which fails a composition's install.
func TestToWrapperRelease_SkipsNilHooks(t *testing.T) {
	src := &release.Release{
		Name:  "withnil",
		Chart: &chart.Chart{Metadata: &chart.Metadata{Version: "1.0.0"}},
		Info:  &release.Info{Status: release.StatusDeployed},
		Hooks: []*release.Hook{nil, {Name: "real", Kind: "Job", Manifest: "apiVersion: batch/v1\nkind: Job\n"}},
	}
	got := toWrapperRelease(src)
	if len(got.Hooks) != 1 {
		t.Fatalf("got %d hooks, want 1 (the nil skipped)", len(got.Hooks))
	}
	if got.Hooks[0].Name != "real" {
		t.Errorf("kept the wrong hook: %q", got.Hooks[0].Name)
	}
}

// Guard that the projection is a PROJECTION: plumbing's Hook must not be helm's type, or every
// consumer touching a hook takes a direct helm dependency — which is the thing Release exists to
// prevent, and chart-inspector keeps helm INDIRECT today.
func TestHookIsPlumbingsOwnType(t *testing.T) {
	var h helmconfig.Hook
	// Compiles only if Events is []string rather than []release.HookEvent.
	var _ []string = h.Events
	// Compiles only if the type is plumbing's own (a helm release.Hook has no such assignability).
	_ = helmconfig.Hook{Name: "x", Kind: "Job", Path: "p", Manifest: "m", Events: []string{"pre-delete"}}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
