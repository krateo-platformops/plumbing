//go:build envtest

package helm

import (
	"context"
	"fmt"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// The pre-upgrade hook of neverDoneHookJob, rendered only when .Values.hook is set, so the
// upgrades before it complete and only the last one is held pending.
const gatedNeverDoneHookJob = "{{- if .Values.hook }}\n" + neverDoneHookJob + "{{- end }}\n"

// core-provider#563: a release held pending must be recoverable to its LAST DEPLOYED revision,
// which only the release's history names. History returns every stored revision with its own
// status, ascending, while a later revision is pending.
//
// Ten completed revisions put the deployed one at 10 and the pending one at 11. helm stores
// revisions as Secrets named ...v1, ...v10, ...v11, ...v2, and the apiserver lists them by name,
// so a History that didn't sort would return them out of order.
func TestEnvtest_HistoryNamesTheDeployedRevisionBelowAPendingOne(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": valuedCM("hist", "default"), "templates/hook.yaml": gatedNeverDoneHookJob})
	if _, err := hc.Install(ctx, "hist", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	for i := 2; i <= 10; i++ {
		if _, err := hc.Upgrade(ctx, "hist", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{
			Values: map[string]any{"k": fmt.Sprintf("v%d", i)}}}); err != nil {
			t.Fatalf("upgrade to revision %d: %v", i, err)
		}
	}

	holding := make(chan error, 1)
	go func() {
		_, err := hc.Upgrade(ctx, "hist", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{
			Values: map[string]any{"k": "held", "hook": true}, Timeout: 20 * time.Second}})
		holding <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rel, err := hc.GetRelease(ctx, "hist", &helmconfig.GetConfig{})
		if err == nil && rel != nil && rel.Status == helmconfig.StatusPendingUpgrade {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: revision 11 never went pending-upgrade (last %v, %v)", rel, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	hist, err := hc.History(ctx, "hist", &helmconfig.HistoryConfig{})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 11 {
		t.Fatalf("history has %d revisions, want 11: %v", len(hist), revisionsOf(hist))
	}
	for i, rel := range hist {
		want := helmconfig.StatusSuperseded
		switch i + 1 {
		case 10:
			want = helmconfig.StatusDeployed
		case 11:
			want = helmconfig.StatusPendingUpgrade
		}
		if rel.Revision != i+1 || rel.Status != want {
			t.Fatalf("history[%d] = revision %d %s, want revision %d %s; all: %v", i, rel.Revision, rel.Status, i+1, want, revisionsOf(hist))
		}
		if rel.Name != "hist" || rel.Namespace != "default" {
			t.Fatalf("history[%d] is %s/%s, want default/hist", i, rel.Namespace, rel.Name)
		}
	}
	if got := hist[9].Values["k"]; got != "v10" {
		t.Fatalf("the deployed revision's values: k = %v, want v10", got)
	}

	// Control: the holding upgrade fails on its own hook timeout, so the release was genuinely
	// pending while History read it.
	if herr := <-holding; herr == nil {
		t.Fatal("control: the holding upgrade must fail on its hook timeout")
	}
}

// A release with no revisions is nil, nil, as GetRelease; the Namespace override is the one
// looked in, and a release in another namespace is not found in the client's own.
func TestEnvtest_HistoryNotFoundAndNamespaceOverride(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()

	hist, err := hc.History(ctx, "never-installed", nil)
	if err != nil || hist != nil {
		t.Fatalf("history of a release that doesn't exist = %v, %v; want nil, nil", hist, err)
	}

	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": valuedCM("elsewhere", "other")})
	if _, err := hc.Install(ctx, "elsewhere", url, &helmconfig.InstallConfig{Namespace: "other", CreateNamespace: true}); err != nil {
		t.Fatalf("install in namespace other: %v", err)
	}

	hist, err = hc.History(ctx, "elsewhere", &helmconfig.HistoryConfig{Namespace: "other"})
	if err != nil {
		t.Fatalf("history with the namespace override: %v", err)
	}
	if len(hist) != 1 || hist[0].Revision != 1 || hist[0].Status != helmconfig.StatusDeployed || hist[0].Namespace != "other" {
		t.Fatalf("history in namespace other = %v, want revision 1 deployed in other", revisionsOf(hist))
	}

	hist, err = hc.History(ctx, "elsewhere", &helmconfig.HistoryConfig{})
	if err != nil || hist != nil {
		t.Fatalf("history in the client's own namespace of a release in another = %v, %v; want nil, nil", revisionsOf(hist), err)
	}
}

// revisionsOf renders a history for a failure message.
func revisionsOf(hist []*helmconfig.Release) []string {
	out := make([]string, 0, len(hist))
	for _, r := range hist {
		out = append(out, fmt.Sprintf("%s/%s@%d:%s", r.Namespace, r.Name, r.Revision, r.Status))
	}
	return out
}
