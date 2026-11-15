//go:build envtest

package helm

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// otherWriter plays a second writer on one child: after every GET of it that Reconcile sends, it
// writes an annotation the chart does not set, with a new value each time. On krateo-057 that writer
// was the child composition's own controller recording krateo.io/traceparent-continued as it joined
// a fresh trace; the parent's Reconcile counted the marker as its own change and wrote a revision
// every pass, ten in a row after one roll (core-provider, installer-platform, 2026-11-15).
type otherWriter struct {
	next   http.RoundTripper
	path   string
	armed  atomic.Bool
	writes atomic.Int64
	cs     kubernetes.Interface
	ns     string
	name   string
}

func (w *otherWriter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := w.next.RoundTrip(r)
	if err == nil && w.armed.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, w.path) {
		n := w.writes.Add(1)
		patch := fmt.Sprintf(`{"metadata":{"annotations":{"example.io/other-writer":"%d"}}}`, n)
		if _, perr := w.cs.CoreV1().ConfigMaps(w.ns).Patch(context.Background(), w.name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); perr != nil {
			return nil, fmt.Errorf("other writer: %w", perr)
		}
	}
	return resp, err
}

func TestEnvtest_ReconcileDoesNotCountAnotherWritersFieldAsItsChange(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	base, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	w := &otherWriter{path: "/namespaces/default/configmaps/cw", cs: kubernetes.NewForConfigOrDie(base), ns: "default", name: "cw"}
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
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": valuedCM("cw", "default")})
	if _, err := hc.Install(ctx, "cw", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	revision := func() int {
		t.Helper()
		rel, err := hc.GetRelease(ctx, "cw", &helmconfig.GetConfig{})
		if err != nil || rel == nil {
			t.Fatalf("get release: %v", err)
		}
		return rel.Revision
	}
	reconcile := func(vals map[string]any) *helmconfig.ReconcileResult {
		t.Helper()
		res, err := hc.Reconcile(ctx, "cw", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: vals}})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		return res
	}

	// 1. Nothing in the chart changed; only the other writer moved the child during the pass.
	w.armed.Store(true)
	before := revision()
	res := reconcile(nil)
	if w.writes.Load() == 0 {
		t.Fatal("control: the other writer never fired, so this case measured nothing")
	}
	if res.Changed || revision() != before || len(res.ChangedObjects) != 0 {
		t.Fatalf("another writer's annotation counted as Reconcile's change: Changed=%v, revision %d -> %d, changedObjects %v, changedPaths %v",
			res.Changed, before, revision(), res.ChangedObjects, res.ChangedPaths)
	}
	// The per-child verdicts (audit records) must agree: no write is recorded for the child.
	for _, v := range res.Objects {
		if v.Name == "cw" {
			t.Fatalf("a verdict was recorded for the other writer's change: %+v", v)
		}
	}

	// 2. Control, with the other writer still writing: a real change to a field the chart sets is
	// still a change, and its paths name only that field.
	res = reconcile(map[string]any{"k": "changed"})
	if !res.Changed || revision() != before+1 {
		t.Fatalf("a real value change: Changed=%v, revision %d -> %d; want a revision", res.Changed, before, revision())
	}
	for child, paths := range res.ChangedPaths {
		for _, p := range paths {
			if strings.Contains(p, "other-writer") {
				t.Errorf("%s: changedPaths names the other writer's field %q", child, p)
			}
		}
	}
}
