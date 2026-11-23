//go:build envtest

package helm

import (
	"context"
	"fmt"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// core-provider#636, on a real apiserver and real release storage. A first install that keeps failing
// is retried with Replace. Each retry is a new revision, and only InstallConfig.MaxHistory bounds them.
//
// An object the failed attempt created is the SAME object afterwards (adopted, not recreated), and a
// retry that succeeds runs the INSTALL hooks. Every attempt here renders the same manifest, so this says
// nothing about CHANGED values: a bare Replace leaves behind what only the failed attempt had (helm's
// requireAdoption merges with original == target; plumbing#231's review measured it).
func TestEnvtest_InstallWithReplaceBoundsItsHistory(t *testing.T) {
	const retries = 5
	for _, tc := range []struct {
		name       string
		maxHistory int
		want       int
	}{
		{"limited to 3", 3, 3},
		{"control: no limit keeps every attempt", 0, retries + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hc := envtestClient(t)
			ctx := context.Background()
			kube, err := kubernetes.NewForConfig(hc.restConfig)
			if err != nil {
				t.Fatal(err)
			}
			// bad.yaml lands in a namespace that does not exist until the fix flag moves it, so the
			// install fails AFTER creating ok.yaml.
			url := serveFiles(t, map[string]string{
				"Chart.yaml":         chartYAML,
				"templates/ok.yaml":  valuedCM("ih-ok", "default"),
				"templates/bad.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ih-bad\n  namespace: {{ .Values.ns | default \"no-such-namespace\" }}\ndata:\n  k: v\n",
				"templates/hook.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ih-hook\n  namespace: default\n" +
					"  annotations:\n    helm.sh/hook: post-install\ndata:\n  k: v\n",
			})
			install := func(values map[string]any) (*helmconfig.OperationResult, error) {
				return hc.Install(ctx, "ih", url, &helmconfig.InstallConfig{Namespace: "default", MaxHistory: tc.maxHistory,
					ActionConfig: &helmconfig.ActionConfig{Replace: true, TakeOwnership: true, Values: values}})
			}
			okUID := ""
			for i := 0; i <= retries; i++ {
				if _, err := install(nil); err == nil {
					t.Fatalf("attempt %d: control: the install must fail", i+1)
				}
				cm, err := kube.CoreV1().ConfigMaps("default").Get(ctx, "ih-ok", metav1.GetOptions{})
				if err != nil {
					t.Fatalf("attempt %d: control: the failed install must have created ih-ok: %v", i+1, err)
				}
				if okUID == "" {
					okUID = string(cm.UID)
				} else if okUID != string(cm.UID) {
					t.Fatalf("attempt %d: ih-ok was recreated (%s, was %s); Replace must adopt it", i+1, cm.UID, okUID)
				}
			}
			history, err := hc.History(ctx, "ih", &helmconfig.HistoryConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != tc.want {
				t.Fatalf("%d revisions after %d attempts, want %d: %v", len(history), retries+1, tc.want, revisions(history))
			}
			for _, r := range history {
				if r.Status != helmconfig.StatusFailed {
					t.Fatalf("control: revision %d is %s, want failed", r.Revision, r.Status)
				}
			}
			if last := history[len(history)-1].Revision; last != retries+1 {
				t.Fatalf("the latest revision is %d, want %d: pruning must drop the OLDEST", last, retries+1)
			}
			if _, err := kube.CoreV1().ConfigMaps("default").Get(ctx, "ih-hook", metav1.GetOptions{}); err == nil {
				t.Fatal("control: the post-install hook ran although no install succeeded")
			}

			op, err := install(map[string]any{"ns": "default"})
			if err != nil {
				t.Fatalf("the retry that can succeed: %v", err)
			}
			if op.Release == nil || op.Release.Status != helmconfig.StatusDeployed || op.Release.Revision != retries+2 {
				t.Fatalf("release = %+v, want revision %d deployed", op.Release, retries+2)
			}
			if _, err := kube.CoreV1().ConfigMaps("default").Get(ctx, "ih-hook", metav1.GetOptions{}); err != nil {
				t.Fatalf("the successful retry must run the post-install hook: %v", err)
			}
			cm, err := kube.CoreV1().ConfigMaps("default").Get(ctx, "ih-ok", metav1.GetOptions{})
			if err != nil || string(cm.UID) != okUID {
				t.Fatalf("ih-ok after the successful retry: err=%v, same object=%v", err, cm != nil && string(cm.UID) == okUID)
			}
		})
	}
}

func revisions(h []*helmconfig.Release) []string {
	var out []string
	for _, r := range h {
		out = append(out, fmt.Sprintf("%d:%s", r.Revision, r.Status))
	}
	return out
}
