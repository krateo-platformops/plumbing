//go:build envtest

package helm

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// interrupt leaves rel pending-install at revision 1: the process dies in the first install, after it
// created the object named last.
func (e *retryEnv) interrupt(t *testing.T, rel, url, last string) {
	t.Helper()
	rc, dead := killer(e.hc.restConfig, func(r *http.Request) (bool, bool) {
		return false, r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/configmaps") && bodyHas(r, last)
	})
	if _, err := e.hc.Install(context.Background(), rel, url, &helmconfig.InstallConfig{Namespace: "default", RestConfig: rc}); err == nil || !dead.Load() {
		t.Fatalf("control: the install must be cut (err=%v)", err)
	}
	if got := e.last(t, rel); got.Status != helmconfig.StatusPendingInstall || got.Revision != 1 {
		t.Fatalf("control: revision %d %s", got.Revision, got.Status)
	}
}

func TestEnvtest_SettleInterruptedInstall(t *testing.T) {
	e := newRetryEnv(t)
	ctx := context.Background()
	url := serveFiles(t, leftoverChart())

	t.Run("an interrupted first install is settled and retried", func(t *testing.T) {
		rel := "first"
		e.interrupt(t, rel, url, rel+"-ok")
		before := e.last(t, rel)
		if _, err := e.retry(rel, url, nil); !errors.Is(err, helmconfig.ErrNotRetryableAsInstall) {
			t.Fatalf("control: a pending install must not be retried before it is settled: %v", err)
		}
		if err := e.hc.SettleInterruptedInstall(ctx, rel, 1); err != nil {
			t.Fatal(err)
		}
		got := e.last(t, rel)
		if got.Status != helmconfig.StatusFailed || got.Revision != 1 || got.Manifest != before.Manifest || len(got.Hooks) != len(before.Hooks) {
			t.Fatalf("settled release = revision %d %s", got.Revision, got.Status)
		}
		ok := e.cm(t, rel+"-ok")
		if ok == nil || e.cm(t, rel+"-post") != nil {
			t.Fatal("control: the cut must fall after the first object and before the post-install hook")
		}
		if _, err := e.retry(rel, url, map[string]any{"edited": true}); err != nil {
			t.Fatal(err)
		}
		if after := e.cm(t, rel+"-ok"); after.UID != ok.UID || len(after.Data) != 1 || e.cm(t, rel+"-orphan") != nil || e.cm(t, rel+"-post") == nil {
			t.Error("the settled install was not retried as an install")
		}
		if _, err := e.hc.Uninstall(ctx, rel, &helmconfig.UninstallConfig{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("what it refuses, writing nothing", func(t *testing.T) {
		e.interrupt(t, "pend", url, "pend-ok")
		e.first(t, "fail", url, map[string]any{"post": "bad"})
		if _, err := e.hc.Install(ctx, "dep", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
			t.Fatal(err)
		}
		// A deployed revision under a pending one: impossible for a first install, so never ours to settle.
		ac, err := e.hc.newActionConfig(ctx, "default", e.hc.restConfig, nil)
		if err != nil {
			t.Fatal(err)
		}
		over, err := ac.Releases.Last("dep")
		if err != nil {
			t.Fatal(err)
		}
		over.Name, over.Version, over.Info.Status = "over", 1, release.StatusDeployed
		if err := ac.Releases.Create(over); err != nil {
			t.Fatal(err)
		}
		over.Version, over.Info.Status = 2, release.StatusPendingInstall
		if err := ac.Releases.Create(over); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			rel string
			rev int
		}{{"absent", 1}, {"pend", 2}, {"pend", 0}, {"fail", 1}, {"dep", 1}, {"over", 2}} {
			rv := e.recordVersions(t)
			if err := e.hc.SettleInterruptedInstall(ctx, c.rel, c.rev); !errors.Is(err, helmconfig.ErrNotInterruptedInstall) {
				t.Errorf("%s revision %d: err = %v, want ErrNotInterruptedInstall", c.rel, c.rev, err)
			}
			if after := e.recordVersions(t); after != rv {
				t.Errorf("%s revision %d: a record was written", c.rel, c.rev)
			}
		}
		if err := e.hc.SettleInterruptedInstall(ctx, "pend", 1); err != nil {
			t.Errorf("control: the pending one must settle: %v", err)
		}
	})

	t.Run("an install that records first is not overwritten", func(t *testing.T) {
		rel := "alive"
		e.interrupt(t, rel, url, rel+"-ok")
		ac, err := e.hc.newActionConfig(ctx, "default", e.hc.restConfig, nil)
		if err != nil {
			t.Fatal(err)
		}
		raced := *e.hc
		raced.restConfig = rest.CopyConfig(e.hc.restConfig)
		done := false
		raced.restConfig.Wrap(func(next http.RoundTripper) http.RoundTripper {
			return roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPut && !done {
					done = true
					l, err := ac.Releases.Last(rel)
					if err != nil {
						t.Error(err)
					} else {
						l.Info.Status = release.StatusDeployed
						if err := ac.Releases.Update(l); err != nil {
							t.Error(err)
						}
					}
				}
				return next.RoundTrip(r)
			})
		})
		if err := raced.SettleInterruptedInstall(ctx, rel, 1); !errors.Is(err, helmconfig.ErrNotInterruptedInstall) || !done {
			t.Fatalf("err = %v (raced=%v), want ErrNotInterruptedInstall", err, done)
		}
		if got := e.last(t, rel); got.Status != helmconfig.StatusDeployed {
			t.Errorf("status %s: the live install's record was overwritten", got.Status)
		}
	})

	t.Run("another storage driver is an error", func(t *testing.T) {
		t.Setenv("HELM_DRIVER", "memory")
		if err := e.hc.SettleInterruptedInstall(ctx, "any", 1); err == nil || errors.Is(err, helmconfig.ErrNotInterruptedInstall) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("ConfigMaps storage", func(t *testing.T) {
		t.Setenv("HELM_DRIVER", "configmap")
		rel := "cmdriver"
		e.interrupt(t, rel, url, "name\":\""+rel+"-ok")
		if err := e.hc.SettleInterruptedInstall(ctx, rel, 1); err != nil {
			t.Fatal(err)
		}
		if got := e.last(t, rel); got.Status != helmconfig.StatusFailed {
			t.Errorf("status %s", got.Status)
		}
	})
}

// recordVersions is every release record's name and resourceVersion.
func (e *retryEnv) recordVersions(t *testing.T) string {
	t.Helper()
	l, err := e.kube.CoreV1().Secrets("default").List(context.Background(), metav1.ListOptions{LabelSelector: "owner=helm"})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, s := range l.Items {
		b.WriteString(s.Name + "@" + s.ResourceVersion + "\n")
	}
	return b.String()
}
