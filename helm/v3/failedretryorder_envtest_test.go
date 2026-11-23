//go:build envtest

package helm

import (
	"context"
	"errors"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// plumbing#232: Reconcile retries a failed release whose desired state did not change through helm
// upgrade alone, so nothing is applied before the pre hooks. The hook fails until its namespace exists.
func TestEnvtest_FailedReleaseRetryRunsPreHooksFirst(t *testing.T) {
	hc := envtestClient(t)
	ctx := context.Background()
	kube, err := kubernetes.NewForConfig(hc.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	value := func(name string) string {
		cm, err := kube.CoreV1().ConfigMaps("default").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return ""
		}
		return cm.Data["k"]
	}
	url := serveFiles(t, map[string]string{
		"Chart.yaml":        chartYAML,
		"templates/ok.yaml": valuedCM("{{ .Release.Name }}", "default"),
		"templates/pre.yaml": "{{- if .Values.gated }}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: pre\n  namespace: {{ .Release.Name }}-ns\n" +
			"  annotations:\n    helm.sh/hook: pre-install,pre-upgrade\n    helm.sh/hook-delete-policy: before-hook-creation\n{{- end }}\n",
	})
	gated := &helmconfig.ActionConfig{Values: map[string]any{"gated": true, "k": "w"}}

	for _, c := range []struct {
		name, rel, before string
		fail              func(t *testing.T, rel string) error
	}{
		{"never deployed", "never", "", func(t *testing.T, rel string) error {
			_, err := hc.Install(ctx, rel, url, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: gated})
			return err
		}},
		{"deployed once", "once", "v", func(t *testing.T, rel string) error {
			if _, err := hc.Install(ctx, rel, url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
				t.Fatal(err)
			}
			_, err := hc.Upgrade(ctx, rel, url, &helmconfig.UpgradeConfig{ActionConfig: gated})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.fail(t, c.rel) == nil {
				t.Fatal("control: the pre hook must fail")
			}
			if got := value(c.rel); got != c.before {
				t.Fatalf("control: before the retry k = %q, want %q", got, c.before)
			}
			recorded := errors.New("not recorded")
			if res, err := hc.Reconcile(ctx, c.rel, url, &helmconfig.UpgradeConfig{ActionConfig: gated,
				BeforeRevision: func(context.Context) error { return recorded }}); !errors.Is(err, recorded) || res.Changed || value(c.rel) != c.before {
				t.Fatalf("unrecorded retry: err=%v changed=%v k=%q; want nothing written", err, res.Changed, value(c.rel))
			}
			res, err := hc.Reconcile(ctx, c.rel, url, &helmconfig.UpgradeConfig{ActionConfig: gated})
			if err == nil || res == nil {
				t.Fatalf("control: the hook must fail again (res=%v)", res)
			}
			if got := value(c.rel); got != c.before {
				t.Errorf("k = %q: the objects were applied before the pre hook", got)
			}
			if !res.RetriedFailedRelease || res.Changed || res.DesiredStateChange != "" || res.Release == nil || res.Release.Status != helmconfig.StatusFailed {
				t.Errorf("result = %+v", res)
			}

			if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: c.rel + "-ns"}}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			res, err = hc.Reconcile(ctx, c.rel, url, &helmconfig.UpgradeConfig{ActionConfig: gated})
			if err != nil || !res.RetriedFailedRelease || !res.Changed || res.Release.Status != helmconfig.StatusDeployed || value(c.rel) != "w" {
				t.Fatalf("the retry that can succeed: err=%v res=%+v k=%q", err, res, value(c.rel))
			}
			if res, err = hc.Reconcile(ctx, c.rel, url, &helmconfig.UpgradeConfig{ActionConfig: gated}); err != nil || res.Changed || res.RetriedFailedRelease {
				t.Errorf("steady state: err=%v res=%+v", err, res)
			}
		})
	}
}
