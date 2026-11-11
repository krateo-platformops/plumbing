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
	"sort"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// serveVerdictChart packages a chart with three release children, one of them kept on uninstall by
// helm.sh/resource-policy: keep, and one hook. All ConfigMaps, so nothing needs a controller to
// become ready on an envtest apiserver.
func serveVerdictChart(t *testing.T) string {
	t.Helper()
	cm := func(name, annotations string) string {
		return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n" + annotations + "data:\n  k: v\n"
	}
	files := []struct{ name, body string }{
		{"verdicts/Chart.yaml", "apiVersion: v2\nname: verdicts\nversion: 0.1.0\n"},
		{"verdicts/templates/a.yaml", cm("verdict-a", "")},
		{"verdicts/templates/b.yaml", cm("verdict-b", "")},
		{"verdicts/templates/keep.yaml", cm("verdict-keep", "  annotations:\n    helm.sh/resource-policy: keep\n")},
		{"verdicts/templates/hook.yaml", cm("verdict-hook", "  annotations:\n    helm.sh/hook: post-install\n")},
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			t.Fatalf("tar header %s: %v", f.name, err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatalf("tar write %s: %v", f.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tgz := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/verdicts-0.1.0.tgz"
}

func verdictsByName(vs []helmconfig.ObjectVerdict) map[string]helmconfig.ObjectVerdict {
	m := make(map[string]helmconfig.ObjectVerdict, len(vs))
	for _, v := range vs {
		m[v.Name] = v
	}
	return m
}

func names(vs []helmconfig.ObjectVerdict) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name+":"+string(v.Change))
	}
	sort.Strings(out)
	return out
}

// plumbing#135, through helm's REAL install and uninstall actions on a real apiserver: the install
// reports each release child created with its uid and leaves the hook out (counted); the uninstall
// reports two deleted and the resource-policy: keep child KEPT, which, without the verdict, nobody
// could tell from deleted. A control reads the kept object back: it is really still there.
func TestEnvtest_InstallAndUninstallReportPerChildVerdicts(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()

	cc, err := NewCachedClients(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := NewClient(cfg, WithNamespace("default"), WithCache(), WithCachedClients(&cc))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer func() { _ = hc.Close() }()
	chartURL := serveVerdictChart(t)

	op, err := hc.Install(context.Background(), "verdicts", chartURL, &helmconfig.InstallConfig{Namespace: "default"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !op.ObjectsComplete {
		t.Error("the install must be fully observed")
	}
	if got, want := names(op.Objects), []string{"verdict-a:created", "verdict-b:created", "verdict-keep:created"}; !equalStrings(got, want) {
		t.Fatalf("install verdicts = %v, want %v", got, want)
	}
	for _, v := range op.Objects {
		if v.UID == "" || v.Resource != "configmaps" || v.Verb != "create" {
			t.Errorf("install verdict %+v: want the server's uid, resource configmaps, verb create", v)
		}
	}
	// Two hook WRITES for one hook object: helm's default delete policy, before-hook-creation,
	// sends a delete of the hook before creating it. Both are excluded from Objects and counted.
	if op.HooksExcluded != 2 {
		t.Errorf("HooksExcluded = %d, want 2 (before-hook-creation delete, then the create of the post-install hook)", op.HooksExcluded)
	}

	un, err := hc.Uninstall(context.Background(), "verdicts", &helmconfig.UninstallConfig{})
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !un.ObjectsComplete {
		t.Error("the uninstall must be fully observed")
	}
	if got, want := names(un.Objects), []string{"verdict-a:deleted", "verdict-b:deleted", "verdict-keep:kept"}; !equalStrings(got, want) {
		t.Fatalf("uninstall verdicts = %v, want %v", got, want)
	}
	if v := verdictsByName(un.Objects)["verdict-keep"]; v.Verb != "delete" {
		t.Errorf("a kept child carries the verb the release asked for (delete), got %q", v.Verb)
	}

	// Control: the kept verdict describes the cluster. The object is still live, and the deleted
	// ones are gone.
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kc.CoreV1().ConfigMaps("default").Get(context.Background(), "verdict-keep", metav1.GetOptions{}); err != nil {
		t.Errorf("the kept ConfigMap must still exist after uninstall: %v", err)
	}
	if _, err := kc.CoreV1().ConfigMaps("default").Get(context.Background(), "verdict-a", metav1.GetOptions{}); err == nil {
		t.Error("control: verdict-a was reported deleted but is still live")
	}
}
