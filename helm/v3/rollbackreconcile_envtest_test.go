//go:build envtest

package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// plumbing#139, on a real apiserver: Rollback reports its writes, Reconcile keeps the verdicts of a
// failed first install, and Reconcile folds its real Upgrade's result (here witnessed by the hook it
// runs, which only that Upgrade can send) into its own.

// serveFiles packages files (chart-relative paths under "c/") as a chart archive served over HTTP.
func serveFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "c/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tgz := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/c-0.1.0.tgz"
}

const chartYAML = "apiVersion: v2\nname: c\nversion: 0.1.0\n"

func valuedCM(name, ns string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n  namespace: " + ns + "\ndata:\n  k: {{ .Values.k | default \"v\" | quote }}\n"
}

func envtestClient(t *testing.T) *client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cc, err := NewCachedClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := NewClient(cfg, WithNamespace("default"), WithCache(), WithCachedClients(&cc))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(func() { _ = hc.Close() })
	return hc
}

func TestEnvtest_RollbackReportsItsWrites(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/a.yaml": valuedCM("rb-a", "default")})

	if _, err := hc.Install(ctx, "rb", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := hc.Upgrade(ctx, "rb", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"k": "w"}}}); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	op, err := hc.Rollback(ctx, "rb", &helmconfig.RollbackConfig{ReleaseVersion: 1})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if op.Release == nil || op.Release.Revision != 3 {
		t.Fatalf("rollback release = %+v, want revision 3", op.Release)
	}
	if !op.ObjectsComplete {
		t.Error("the rollback must be fully observed")
	}
	// Back from k=w to k=v: the ConfigMap's content changed, so its verdict is specChanged.
	if got, want := changes(op.Objects), []string{"rb-a:specChanged:patch"}; !equalStrings(got, want) {
		t.Errorf("rollback verdicts = %v, want %v", got, want)
	}
}

func TestEnvtest_ReconcileKeepsAFailedFirstInstallsVerdicts(t *testing.T) {
	hc := envtestClient(t)
	// No stored release, so Reconcile installs through Upgrade. One child lands in a namespace that
	// does not exist, so the install fails, after creating the other.
	url := serveFiles(t, map[string]string{
		"Chart.yaml":         chartYAML,
		"templates/ok.yaml":  valuedCM("first-ok", "default"),
		"templates/bad.yaml": valuedCM("first-bad", "no-such-namespace"),
	})
	res, err := hc.Reconcile(context.Background(), "first", url, &helmconfig.UpgradeConfig{})
	if err == nil {
		t.Fatal("the install must fail: one child's namespace does not exist")
	}
	if res == nil {
		t.Fatalf("a failed first install that wrote children must return their verdicts, got nil (err: %v)", err)
	}
	// Sorted by identity: namespace, then name.
	if got, want := changes(res.Objects), []string{"first-ok:created:create", "first-bad:createFailed:create"}; !equalStrings(got, want) {
		t.Errorf("verdicts = %v, want %v", got, want)
	}
	if res.Changed {
		t.Error("a failed reconcile reports Changed=false")
	}
}

func TestEnvtest_ReconcileFoldsItsUpgradesResult(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{
		"Chart.yaml":       chartYAML,
		"templates/a.yaml": valuedCM("fold-a", "default"),
		// A hook only the real Upgrade sends: the self-heal merge runs no hook.
		"templates/hook.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fold-hook\n  namespace: default\n" +
			"  annotations:\n    helm.sh/hook: post-upgrade\n    helm.sh/hook-delete-policy: before-hook-creation\ndata:\n  k: v\n",
	})
	if _, err := hc.Install(ctx, "fold", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// A values change: the merge patches fold-a, detects the change, and runs the real Upgrade,
	// which runs the post-upgrade hook.
	res, err := hc.Reconcile(ctx, "fold", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"k": "w"}}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Changed {
		t.Fatal("control: a values change must be detected, or step 7 never runs")
	}
	if res.HooksExcluded == 0 {
		t.Error("the real Upgrade's hook writes must be folded into the result (HooksExcluded = 0)")
	}
	// The merge's verdict is there; the upgrade, after a converged merge, changed nothing more.
	if got, want := changes(res.Objects), []string{"fold-a:specChanged:patch"}; !equalStrings(got, want) {
		t.Errorf("verdicts = %v, want %v", got, want)
	}
	if !res.ObjectsComplete {
		t.Error("both phases were observed, so the result is complete")
	}
}

// The no-stored-release path INSTALLS (plumbing#139). It used to run helm's Upgrade action, which
// never installs, so it failed on every chart with "has no deployed releases" and wrote nothing.
func TestEnvtest_ReconcileInstallsWhenNoReleaseIsStored(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/ok.yaml": valuedCM("fresh-ok", "default")})
	res, err := hc.Reconcile(ctx, "fresh", url, &helmconfig.UpgradeConfig{})
	if err != nil {
		t.Fatalf("a first reconcile of a valid chart must install it: %v", err)
	}
	if !res.Changed || res.Release == nil || res.Release.Revision != 1 {
		t.Errorf("Changed=%v release=%+v, want true and revision 1", res.Changed, res.Release)
	}
	if got, want := changes(res.Objects), []string{"fresh-ok:created:create"}; !equalStrings(got, want) {
		t.Errorf("verdicts = %v, want %v", got, want)
	}
	// Control: the release is really stored, so the next reconcile self-heals against it.
	rel, err := hc.GetRelease(ctx, "fresh", &helmconfig.GetConfig{})
	if err != nil || rel == nil {
		t.Fatalf("the release must be stored after the first reconcile: rel=%v err=%v", rel, err)
	}
}

// core-provider#465, on a real apiserver: a rollback whose only change to a child is an authorization
// LABEL is specChanged when RollbackConfig declares that label, and metadataOnly when it declares
// nothing (the control). This drives Rollback's own call sites, not only the accessor.
func TestEnvtest_RollbackCountsDeclaredLabels(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	const principal = "authn.krateo.io/principal"
	url := serveFiles(t, map[string]string{
		"Chart.yaml": chartYAML,
		"templates/a.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: rb-label\n  namespace: default\n" +
			"  labels:\n    " + principal + ": {{ .Values.who | default \"alice\" | quote }}\ndata:\n  k: v\n",
	})
	if _, err := hc.Install(ctx, "rblabel", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	upgradeTo := func(who string) {
		t.Helper()
		if _, err := hc.Upgrade(ctx, "rblabel", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"who": who}}}); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
	}
	verdictOf := func(op *helmconfig.OperationResult) helmconfig.ObjectChange {
		t.Helper()
		for _, v := range op.Objects {
			if v.Name == "rb-label" {
				return v.Change
			}
		}
		t.Fatalf("no verdict for rb-label in %v", changes(op.Objects))
		return ""
	}

	// Revision 2 (bob), then roll back to 1 (alice) with the label declared: specChanged.
	upgradeTo("bob")
	op, err := hc.Rollback(ctx, "rblabel", &helmconfig.RollbackConfig{ReleaseVersion: 1, ContentLabels: []string{principal}})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := verdictOf(op); got != helmconfig.ChangeSpec {
		t.Errorf("a label-only rollback with the label declared: %q, want specChanged", got)
	}

	// Control: back to bob, then the same rollback declaring nothing: metadataOnly.
	upgradeTo("bob")
	op, err = hc.Rollback(ctx, "rblabel", &helmconfig.RollbackConfig{ReleaseVersion: 1})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := verdictOf(op); got != helmconfig.ChangeMetadataOnly {
		t.Errorf("control: the same rollback declaring nothing: %q, want metadataOnly", got)
	}
}
