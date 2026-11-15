//go:build envtest

package helm

import (
	"context"
	"errors"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
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
	cfg := func(vals map[string]any, hook func(context.Context) error) *helmconfig.UpgradeConfig {
		return &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: vals}, BeforeRevision: hook}
	}

	// 1. Steady state: nothing changed, no revision, the hook is not called.
	calls := 0
	count := func(context.Context) error { calls++; return nil }
	before := revision()
	if _, err := hc.Reconcile(ctx, "br", url, cfg(nil, count)); err != nil {
		t.Fatalf("steady reconcile: %v", err)
	}
	if calls != 0 || revision() != before {
		t.Fatalf("steady state: hook called %d times, revision %d -> %d; want 0 calls, no revision", calls, before, revision())
	}

	// 2. A value change writes a revision: the hook is called exactly once, BEFORE the revision.
	var revAtHook int
	hook := func(context.Context) error { calls++; revAtHook = revision(); return nil }
	if _, err := hc.Reconcile(ctx, "br", url, cfg(map[string]any{"k": "changed"}, hook)); err != nil {
		t.Fatalf("changing reconcile: %v", err)
	}
	if calls != 1 || revAtHook != before || revision() != before+1 {
		t.Fatalf("a revision: hook called %d times seeing revision %d, now %d; want 1 call, seeing %d, then %d", calls, revAtHook, revision(), before, before+1)
	}

	// 3. core-provider#526: a hook that cannot record the operation ABORTS it. Nothing is written:
	// no revision, and the changed value never reaches the live object.
	refused := errors.New("record write conflicted")
	before = revision()
	_, err := hc.Reconcile(ctx, "br", url, cfg(map[string]any{"k": "unrecorded"}, func(context.Context) error { return refused }))
	if !errors.Is(err, refused) {
		t.Fatalf("a hook error: err = %v, want it returned wrapped", err)
	}
	cm, gerr := kubernetes.NewForConfigOrDie(hc.restConfig).CoreV1().ConfigMaps("default").Get(ctx, "br", metav1.GetOptions{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if revision() != before || cm.Data["k"] == "unrecorded" {
		t.Fatalf("an unrecorded operation ran: revision %d -> %d, live k=%q", before, revision(), cm.Data["k"])
	}

	// 4. The merge path's revision (step 7) is not written unrecorded either: drift against the
	// deployed state is healed by the merge, but the revision waits for a recorded pass.
	cs := kubernetes.NewForConfigOrDie(hc.restConfig)
	if _, err := cs.CoreV1().ConfigMaps("default").Patch(ctx, "br", types.MergePatchType, []byte(`{"data":{"k":"drifted"}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	before = revision()
	stored, _ := hc.GetRelease(ctx, "br", &helmconfig.GetConfig{})
	_, err = hc.Reconcile(ctx, "br", url, cfg(stored.Values, func(context.Context) error { return refused }))
	if !errors.Is(err, refused) || revision() != before {
		t.Fatalf("drift with an unrecordable revision: err = %v, revision %d -> %d; want the error and no revision", err, before, revision())
	}

}
