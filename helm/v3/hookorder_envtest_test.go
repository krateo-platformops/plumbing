//go:build envtest

package helm

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// writeLog records the mutating requests a client sends, in order.
type writeLog struct {
	next http.RoundTripper
	mu   sync.Mutex
	on   bool
	seq  []string
}

func (w *writeLog) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet {
		w.mu.Lock()
		if w.on {
			w.seq = append(w.seq, r.Method+" "+r.URL.Path)
		}
		w.mu.Unlock()
	}
	return w.next.RoundTrip(r)
}

func (w *writeLog) first(substr string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, s := range w.seq {
		if strings.Contains(s, substr) {
			return i
		}
	}
	return -1
}

const preUpgradeHookCM = `apiVersion: v1
kind: ConfigMap
metadata:
  name: pre
  namespace: default
  annotations:
    helm.sh/hook: pre-upgrade
    helm.sh/hook-delete-policy: before-hook-creation
data:
  k: {{ .Values.k | default "v" | quote }}
`

// core-provider#518: a changed desired state (here a value) reaches the release's objects only
// through helm upgrade, so a pre-upgrade hook runs BEFORE the objects change. Reconcile used to
// merge-apply the new render first (its step 5) and run the upgrade, hooks included, afterwards: on
// GKE a Keystone credential's pre-upgrade Job ran three minutes after the workloads reading it had
// restarted on the new config (openstack-as-a-service, nova 0.3.62 -> 0.3.64).
func TestEnvtest_ReconcileRunsPreUpgradeHooksBeforeAChangedRenderIsApplied(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	base, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	w := &writeLog{}
	cfg := *base
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { w.next = rt; return w }
	cc, err := NewCachedClients(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := NewClient(&cfg, WithNamespace("default"), WithCache(), WithCachedClients(&cc))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hc.Close() })

	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": valuedCM("app", "default"), "templates/hook.yaml": preUpgradeHookCM})
	if _, err := hc.Install(ctx, "ho", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	w.mu.Lock()
	w.on = true
	w.mu.Unlock()
	res, err := hc.Reconcile(ctx, "ho", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"k": "changed"}}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Changed || res.DesiredStateChange != "rendered manifest" {
		t.Fatalf("a value change: Changed=%v desiredStateChange %q, want true and \"rendered manifest\"", res.Changed, res.DesiredStateChange)
	}
	if len(res.ChangedObjects) != 1 || res.ChangedObjects[0] != "v1/ConfigMap/default/app" {
		t.Fatalf("changedObjects = %v, want the one changed child v1/ConfigMap/default/app", res.ChangedObjects)
	}
	hook, app := w.first("/configmaps/pre"), w.first("/configmaps/app")
	if hook < 0 || app < 0 {
		t.Fatalf("control: the write log must see both the hook and the object written (hook %d, app %d): %v", hook, app, w.seq)
	}
	if app < hook {
		t.Fatalf("the object changed BEFORE its pre-upgrade hook ran: writes %v", w.seq)
	}
}

// Pure drift against an unchanged desired state still self-heals by merge and writes no revision:
// the fix must not turn every drift into an upgrade with hooks.
func TestEnvtest_ReconcileStillHealsPureDriftWithoutAnUpgrade(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": valuedCM("dr", "default"), "templates/hook.yaml": preUpgradeHookCM})
	if _, err := hc.Install(ctx, "dr", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	rel, _ := hc.GetRelease(ctx, "dr", &helmconfig.GetConfig{})
	before := rel.Revision
	res, err := hc.Reconcile(ctx, "dr", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	rel, _ = hc.GetRelease(ctx, "dr", &helmconfig.GetConfig{})
	if res.Changed || rel.Revision != before || res.DesiredStateChange != "" {
		t.Fatalf("an unchanged desired state: Changed=%v revision %d -> %d, desiredStateChange %q; want no change", res.Changed, before, rel.Revision, res.DesiredStateChange)
	}
}
