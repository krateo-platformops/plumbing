//go:build envtest

package helm

import (
	"context"
	"errors"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// A pre-upgrade Job never completes on envtest (no Job controller), so helm waits on it until the
// upgrade's timeout: the release sits pending-upgrade for that long, exactly as on a cluster where
// a hook is slow or its controller died mid-operation.
const neverDoneHookJob = `apiVersion: batch/v1
kind: Job
metadata:
  name: slow-hook
  namespace: default
  annotations:
    helm.sh/hook: pre-upgrade
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: c
        image: busybox
`

// core-provider#520: helm's refusal to start while another operation holds the release is
// distinguishable, so a caller can tell "my upgrade never started" from "my upgrade failed".
func TestEnvtest_UpgradeRefusedByAPendingReleaseIsErrOperationInProgress(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": valuedCM("op", "default"), "templates/hook.yaml": neverDoneHookJob})
	if _, err := hc.Install(ctx, "op", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	holding := make(chan error, 1)
	go func() {
		_, err := hc.Upgrade(ctx, "op", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{
			Values: map[string]any{"k": "first"}, Timeout: 20 * time.Second}})
		holding <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		rel, err := hc.GetRelease(ctx, "op", &helmconfig.GetConfig{})
		if err == nil && rel != nil && rel.Status == helmconfig.StatusPendingUpgrade {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: the first upgrade never held the release pending (last %v, %v)", rel, err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	_, err := hc.Upgrade(ctx, "op", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"k": "second"}}})
	if !errors.Is(err, helmconfig.ErrOperationInProgress) {
		t.Fatalf("an upgrade while another holds the release: err = %v, want ErrOperationInProgress", err)
	}

	// Control: the holding upgrade's own failure (its hook timed out) is NOT this error.
	if herr := <-holding; herr == nil || errors.Is(herr, helmconfig.ErrOperationInProgress) {
		t.Fatalf("control: the holding upgrade must fail on its own hook timeout, not as ErrOperationInProgress; got %v", herr)
	}
}
