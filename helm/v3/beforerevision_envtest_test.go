//go:build envtest

package helm

import (
	"context"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// core-provider#461: Reconcile calls UpgradeConfig.BeforeRevision immediately before it writes a
// revision, and never on a reconcile that writes none.
func TestEnvtest_ReconcileCallsBeforeRevisionOnlyBeforeARevision(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": valuedCM("br", "default")})
	if _, err := hc.Install(ctx, "br", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	revision := func() int {
		t.Helper()
		rel, err := hc.GetRelease(ctx, "br", &helmconfig.GetConfig{})
		if err != nil || rel == nil {
			t.Fatalf("get release: %v", err)
		}
		return rel.Revision
	}
	cfg := func(vals map[string]any, hook func(context.Context)) *helmconfig.UpgradeConfig {
		return &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: vals}, BeforeRevision: hook}
	}

	// 1. Steady state: nothing changed, no revision, the hook is not called.
	calls := 0
	count := func(context.Context) { calls++ }
	before := revision()
	if _, err := hc.Reconcile(ctx, "br", url, cfg(nil, count)); err != nil {
		t.Fatalf("steady reconcile: %v", err)
	}
	if calls != 0 || revision() != before {
		t.Fatalf("steady state: hook called %d times, revision %d -> %d; want 0 calls, no revision", calls, before, revision())
	}

	// 2. A value change writes a revision: the hook is called exactly once, BEFORE the revision.
	var revAtHook int
	hook := func(context.Context) { calls++; revAtHook = revision() }
	if _, err := hc.Reconcile(ctx, "br", url, cfg(map[string]any{"k": "changed"}, hook)); err != nil {
		t.Fatalf("changing reconcile: %v", err)
	}
	if calls != 1 || revAtHook != before || revision() != before+1 {
		t.Fatalf("a revision: hook called %d times seeing revision %d, now %d; want 1 call, seeing %d, then %d", calls, revAtHook, revision(), before, before+1)
	}

}
