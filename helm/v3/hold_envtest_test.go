//go:build envtest

package helm

import (
	"context"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// core-provider#519: HoldDesiredStateChange keeps Reconcile from moving the release to a changed
// request, by upgrade or by merge, while still healing drift against the deployed desired state.
func TestEnvtest_ReconcileHoldsADesiredStateChangeButHealsDrift(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": valuedCM("hold", "default")})
	if _, err := hc.Install(ctx, "hold", url, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"k": "deployed"}}}); err != nil {
		t.Fatalf("install: %v", err)
	}
	cs := kubernetes.NewForConfigOrDie(hc.restConfig)
	liveK := func() string {
		cm, err := cs.CoreV1().ConfigMaps("default").Get(ctx, "hold", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return cm.Data["k"]
	}
	revision := func() int {
		rel, _ := hc.GetRelease(ctx, "hold", &helmconfig.GetConfig{})
		return rel.Revision
	}
	hold := func(vals map[string]any) *helmconfig.ReconcileResult {
		t.Helper()
		res, err := hc.Reconcile(ctx, "hold", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: vals}, HoldDesiredStateChange: true})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		return res
	}

	// 1. A changed request is held: nothing written, the change reported.
	before := revision()
	res := hold(map[string]any{"k": "requested"})
	if res.Changed || !res.DesiredStateHeld || res.DesiredStateChange != "values" || revision() != before || liveK() != "deployed" {
		t.Fatalf("a held change: Changed=%v held=%v change=%q revision %d -> %d live k=%q; want nothing applied and the change reported",
			res.Changed, res.DesiredStateHeld, res.DesiredStateChange, before, revision(), liveK())
	}

	// 2. Control: drift against the deployed desired state still heals under the hold.
	if _, err := cs.CoreV1().ConfigMaps("default").Patch(ctx, "hold", types.MergePatchType, []byte(`{"data":{"k":"drifted"}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	res = hold(map[string]any{"k": "deployed"})
	if res.DesiredStateHeld || liveK() != "deployed" {
		t.Fatalf("pure drift under the hold: held=%v live k=%q; want healed back to \"deployed\"", res.DesiredStateHeld, liveK())
	}
}
