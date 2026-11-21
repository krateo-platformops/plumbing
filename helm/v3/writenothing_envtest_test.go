//go:build envtest

package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// core-provider#597: UpgradeConfig.WriteNothing writes nothing, proven by request and by state,
// across every arm of Reconcile that would write, with a real Reconcile as the instrument's control.

type recordedReq struct{ method, path, dryRun string }

type reqRecorder struct {
	mu   sync.Mutex
	on   bool
	reqs []recordedReq
}

func (r *reqRecorder) start() { r.mu.Lock(); r.on, r.reqs = true, nil; r.mu.Unlock() }

func (r *reqRecorder) stop() []recordedReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = false
	return append([]recordedReq(nil), r.reqs...)
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// mutating returns the recorded requests that are not reads, and those of them without dryRun=All.
func mutating(reqs []recordedReq) (all, undry []recordedReq) {
	for _, q := range reqs {
		if isReadMethod(q.method) {
			continue
		}
		all = append(all, q)
		if q.dryRun != metav1.DryRunAll {
			undry = append(undry, q)
		}
	}
	return all, undry
}

type wnEnv struct {
	hc  *client
	cs  kubernetes.Interface
	rec *reqRecorder
	st  *storage.Storage
}

func newWNEnv(t *testing.T) *wnEnv {
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
	rec := &reqRecorder{}
	recorded := rest.CopyConfig(cfg)
	recorded.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return rtFunc(func(r *http.Request) (*http.Response, error) {
			rec.mu.Lock()
			if rec.on {
				dry := r.URL.Query().Get("dryRun")
				// A DELETE's options travel in its body, and the apiserver reads them from there when
				// a body is present: the query alone proves nothing for a DELETE.
				if r.Method == http.MethodDelete && r.Body != nil && r.Body != http.NoBody && r.GetBody != nil {
					if b, err := r.GetBody(); err == nil {
						raw, _ := io.ReadAll(b)
						var opts struct {
							DryRun []string `json:"dryRun"`
						}
						if len(bytes.TrimSpace(raw)) > 0 && (json.Unmarshal(raw, &opts) != nil || !slices.Contains(opts.DryRun, metav1.DryRunAll)) {
							dry = "body:" + string(raw)
						}
					}
				}
				rec.reqs = append(rec.reqs, recordedReq{method: r.Method, path: r.URL.Path, dryRun: dry})
			}
			rec.mu.Unlock()
			return rt.RoundTrip(r)
		})
	})
	cc, err := NewCachedClients(recorded)
	if err != nil {
		t.Fatal(err)
	}
	hc, err := NewClient(recorded, WithNamespace("default"), testCache(t), WithCachedClients(&cc))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hc.Close() })
	cs := kubernetes.NewForConfigOrDie(cfg) // the test's own writes are not recorded
	return &wnEnv{hc: hc, cs: cs, rec: rec, st: storage.Init(driver.NewSecrets(cs.CoreV1().Secrets("default")))}
}

// snapshot is every ConfigMap, Secret (helm's release records among them) and Job in default, by
// identity, as resourceVersion plus content. Any write changes a resourceVersion; any create or
// delete changes the key set.
func (e *wnEnv) snapshot(t *testing.T) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	cms, err := e.cs.CoreV1().ConfigMaps("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range cms.Items {
		b, _ := json.Marshal(o.Data)
		out["ConfigMap/"+o.Name] = o.ResourceVersion + " " + string(b)
	}
	secs, err := e.cs.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range secs.Items {
		b, _ := json.Marshal(o.Data)
		out["Secret/"+o.Name] = o.ResourceVersion + " " + string(b)
	}
	jobs, err := e.cs.BatchV1().Jobs("default").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range jobs.Items {
		out["Job/"+o.Name] = o.ResourceVersion
	}
	return out
}

func sameSnapshot(a, b map[string]string) (bool, string) {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var diff []string
	for k := range keys {
		if a[k] != b[k] {
			diff = append(diff, fmt.Sprintf("%s: %q -> %q", k, a[k], b[k]))
		}
	}
	sort.Strings(diff)
	return len(diff) == 0, strings.Join(diff, "; ")
}

// writeNothing runs one WriteNothing Reconcile and asserts the two proofs: no mutating request
// without dryRun=All, and an identical namespace. It returns the result, the error and how many
// mutating (dry-run) requests it sent.
func (e *wnEnv) writeNothing(t *testing.T, release, url string, vals map[string]any) (*helmconfig.ReconcileResult, error, int) {
	t.Helper()
	before := e.snapshot(t)
	e.rec.start()
	res, err := e.hc.Reconcile(context.Background(), release, url, &helmconfig.UpgradeConfig{
		ActionConfig: &helmconfig.ActionConfig{Values: vals}, WriteNothing: true,
	})
	all, undry := mutating(e.rec.stop())
	if len(undry) > 0 {
		t.Errorf("%s: %d mutating request(s) WITHOUT dryRun=All: %+v", release, len(undry), undry)
	}
	if same, diff := sameSnapshot(before, e.snapshot(t)); !same {
		t.Errorf("%s: WriteNothing changed the namespace: %s", release, diff)
	}
	if res != nil {
		if res.Changed || res.Created+res.Deleted+res.PatchedUpdated > 0 || len(res.ChangedObjects) > 0 || len(res.Objects) > 0 ||
			len(res.IntendedDeletions) > 0 || len(res.KeptDeletions) > 0 || !res.WriteWithheld {
			t.Errorf("%s: a write-recording field is set under WriteNothing: %+v", release, res)
		}
	}
	t.Logf("%s: %d mutating request(s), %d without dryRun=All", release, len(all), len(undry))
	return res, err, len(all)
}

func (e *wnEnv) install(t *testing.T, release, url string, vals map[string]any) {
	t.Helper()
	if _, err := e.hc.Install(context.Background(), release, url, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: &helmconfig.ActionConfig{Values: vals}}); err != nil {
		t.Fatalf("install %s: %v", release, err)
	}
}

func cmYAML(name string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n  namespace: default\ndata:\n  k: {{ .Values.k | quote }}\n"
}

func TestEnvtest_WriteNothing(t *testing.T) {
	e := newWNEnv(t)
	ctx := context.Background()
	v := map[string]any{"k": "v"}
	patchK := func(kind, name, val string) {
		t.Helper()
		var err error
		switch kind {
		case "ConfigMap":
			_, err = e.cs.CoreV1().ConfigMaps("default").Patch(ctx, name, types.MergePatchType, []byte(`{"data":{"k":"`+val+`"}}`), metav1.PatchOptions{})
		case "Secret":
			_, err = e.cs.CoreV1().Secrets("default").Patch(ctx, name, types.MergePatchType, []byte(`{"stringData":{"k":"`+val+`"}}`), metav1.PatchOptions{})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	editStoredManifest := func(release string, edit func(string) string) {
		t.Helper()
		r, err := e.st.Last(release)
		if err != nil {
			t.Fatal(err)
		}
		r.Manifest = edit(r.Manifest)
		if err := e.st.Update(r); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("no stored release: would install", func(t *testing.T) {
		url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": cmYAML("wn-none")})
		res, err, n := e.writeNothing(t, "wn-none", url, v)
		if err != nil || !res.WouldInstall || !res.WouldChange {
			t.Fatalf("got %+v, %v; want WouldInstall", res, err)
		}
		if n != 0 {
			t.Errorf("an absent release sent %d mutating requests, want 0", n)
		}
	})

	t.Run("drift: would patch", func(t *testing.T) {
		url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": cmYAML("wn-drift")})
		e.install(t, "wn-drift", url, v)
		patchK("ConfigMap", "wn-drift", "drifted")
		res, err, n := e.writeNothing(t, "wn-drift", url, v)
		if err != nil || res.WouldPatch != 1 || !res.WouldChange || res.WouldCreate+res.WouldDelete != 0 || n == 0 {
			t.Fatalf("got %+v, %v (%d dry-run requests); want WouldPatch=1 sent as a dry run", res, err, n)
		}
	})

	t.Run("out-of-band delete: would create", func(t *testing.T) {
		url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": cmYAML("wn-del")})
		e.install(t, "wn-del", url, v)
		if err := e.cs.CoreV1().ConfigMaps("default").Delete(ctx, "wn-del", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		res, err, _ := e.writeNothing(t, "wn-del", url, v)
		if err != nil || res.WouldCreate != 1 || !res.WouldChange {
			t.Fatalf("got %+v, %v; want WouldCreate=1 (a dry-run create carries no resourceVersion)", res, err)
		}
	})

	t.Run("desired-state change: reported, nothing sent", func(t *testing.T) {
		url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": cmYAML("wn-change")})
		e.install(t, "wn-change", url, v)
		res, err, _ := e.writeNothing(t, "wn-change", url, map[string]any{"k": "new"})
		if err != nil || res.DesiredStateChange != "values" || !res.WouldChange || res.DesiredStateHeld || res.WouldPatch != 0 {
			t.Fatalf("got %+v, %v; want the change reported, not held, no merge", res, err)
		}
	})

	t.Run("adoption: a live child missing from the stored manifest", func(t *testing.T) {
		url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/a.yaml": cmYAML("wn-adopt-a"), "templates/b.yaml": cmYAML("wn-adopt-b")})
		e.install(t, "wn-adopt", url, v)
		editStoredManifest("wn-adopt", func(m string) string {
			var keep []string
			for _, doc := range strings.Split(m, "\n---") {
				if !strings.Contains(doc, "name: wn-adopt-b") {
					keep = append(keep, doc)
				}
			}
			return strings.Join(keep, "\n---")
		})
		patchK("ConfigMap", "wn-adopt-b", "drifted")
		res, err, _ := e.writeNothing(t, "wn-adopt", url, v)
		if err != nil || res.WouldPatch != 1 || res.WouldCreate != 0 {
			t.Fatalf("got %+v, %v; want the adopted child's drift as WouldPatch=1", res, err)
		}
	})

	t.Run("pending deletion: would delete", func(t *testing.T) {
		url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": cmYAML("wn-pend")})
		e.install(t, "wn-pend", url, v)
		extra := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: wn-pend-extra\n  namespace: default\ndata:\n  k: x\n"
		if _, err := e.cs.CoreV1().ConfigMaps("default").Create(ctx, newCM("wn-pend-extra"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		editStoredManifest("wn-pend", func(m string) string { return m + "\n---\n# Source: c/templates/extra.yaml\n" + extra })
		res, err, _ := e.writeNothing(t, "wn-pend", url, v)
		if err != nil || res.WouldDelete != 1 || !res.WouldChange {
			t.Fatalf("got %+v, %v; want WouldDelete=1", res, err)
		}
		if _, gerr := e.cs.CoreV1().ConfigMaps("default").Get(ctx, "wn-pend-extra", metav1.GetOptions{}); gerr != nil {
			t.Errorf("the child the merge would delete is gone: %v", gerr)
		}
	})

	t.Run("the stored manifest needs a repair: withheld, nothing sent", func(t *testing.T) {
		url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": cmYAML("wn-repair")})
		e.install(t, "wn-repair", url, v)
		unserved := "apiVersion: example.org/v1\nkind: Gadget\nmetadata:\n  name: g\n  namespace: default\n"
		editStoredManifest("wn-repair", func(m string) string { return m + "\n---\n# Source: c/templates/g.yaml\n" + unserved })
		res, err, n := e.writeNothing(t, "wn-repair", url, v)
		if !errors.Is(err, helmconfig.ErrStoredManifestRepairWithheld) || !strings.Contains(err.Error(), "Gadget") || res != nil {
			t.Fatalf("got %+v, %v; want ErrStoredManifestRepairWithheld naming Gadget", res, err)
		}
		if n != 0 {
			t.Errorf("the withheld repair sent %d mutating request(s), want 0 (not even a dry run)", n)
		}
	})

	t.Run("a live Job is kept out of the merge", func(t *testing.T) {
		job := "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: wn-job\n  namespace: default\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n      - name: c\n        image: busybox\n"
		url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": cmYAML("wn-jobcm"), "templates/job.yaml": job})
		e.install(t, "wn-jobs", url, v)
		patchK("ConfigMap", "wn-jobcm", "drifted")
		before := e.snapshot(t)
		e.rec.start()
		res, err := e.hc.Reconcile(ctx, "wn-jobs", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: v}, WriteNothing: true})
		reqs := e.rec.stop()
		all, undry := mutating(reqs)
		for _, q := range all {
			if strings.Contains(q.path, "/jobs/") {
				t.Errorf("a request was sent for the live Job: %+v", q)
			}
		}
		if len(undry) > 0 {
			t.Errorf("mutating requests without dryRun=All: %+v", undry)
		}
		if same, diff := sameSnapshot(before, e.snapshot(t)); !same {
			t.Errorf("changed: %s", diff)
		}
		if err != nil || res.WouldPatch != 1 {
			t.Fatalf("got %+v, %v; want the ConfigMap's drift only", res, err)
		}
	})

	t.Run("partial: a webhook denies one child's dry run", func(t *testing.T) {
		sec := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: wn-part-s\n  namespace: default\nstringData:\n  k: {{ .Values.k | quote }}\n"
		url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": cmYAML("wn-part-c"), "templates/s.yaml": sec})
		e.install(t, "wn-part", url, v)
		patchK("ConfigMap", "wn-part-c", "drifted")
		patchK("Secret", "wn-part-s", "drifted")
		denyDryRunOnConfigMaps(t, e.cs)
		res, err, _ := e.writeNothing(t, "wn-part", url, v)
		if !errors.Is(err, helmconfig.ErrDryRunRefused) || !strings.Contains(err.Error(), "configmaps/wn-part-c") {
			t.Fatalf("err = %v; want ErrDryRunRefused naming the ConfigMap", err)
		}
		if res == nil || res.WouldPatch != 1 || len(res.WouldPatchObjects) != 1 || !strings.Contains(res.WouldPatchObjects[0], "wn-part-s") {
			t.Fatalf("got %+v; want the Secret's drift reported beside the refusal", res)
		}
	})

	// CONTROL for the instrument: the same drift through a REAL Reconcile is seen as mutating
	// requests WITHOUT dryRun, and it does change the namespace. Without this, "zero undry requests"
	// could be a recorder that sees nothing.
	t.Run("control: a real Reconcile is recorded writing", func(t *testing.T) {
		before := e.snapshot(t)
		e.rec.start()
		res, err := e.hc.Reconcile(ctx, "wn-drift", serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": cmYAML("wn-drift")}),
			&helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: v}})
		all, undry := mutating(e.rec.stop())
		same, _ := sameSnapshot(before, e.snapshot(t))
		t.Logf("control: %d mutating request(s), %d without dryRun; namespace identical=%v; Changed=%v", len(all), len(undry), same, res != nil && res.Changed)
		if err != nil || len(undry) == 0 || same || !res.Changed {
			t.Fatalf("control: err=%v undry=%d same=%v; the recorder or the snapshot cannot see a real write", err, len(undry), same)
		}
	})
}

func newCM(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Data: map[string]string{"k": "x"}}
}

// denyDryRunOnConfigMaps registers a validating webhook that denies every dry-run request on
// ConfigMaps (and allows real ones), and waits until the apiserver enforces it.
func denyDryRunOnConfigMaps(t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rev map[string]any
		_ = json.NewDecoder(r.Body).Decode(&rev)
		req, _ := rev["request"].(map[string]any)
		dry, _ := req["dryRun"].(bool)
		resp := map[string]any{"uid": req["uid"], "allowed": !dry}
		if dry {
			resp["status"] = map[string]any{"code": 403, "message": "dry runs are refused here"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview", "response": resp})
	}))
	t.Cleanup(srv.Close)
	none, fail, url := admissionv1.SideEffectClassNone, admissionv1.Fail, srv.URL
	ctx := context.Background()
	if _, err := cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(ctx, &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-dry-run"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name: "deny-dry-run.example.org", SideEffects: &none, FailurePolicy: &fail, AdmissionReviewVersions: []string{"v1"},
			ClientConfig: admissionv1.WebhookClientConfig{URL: &url, CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})},
			Rules: []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.OperationAll},
				Rule: admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"}}}},
		}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // before srv.Close (cleanups run last-in first-out): no later arm meets it
		_ = cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(context.Background(), "deny-dry-run", metav1.DeleteOptions{})
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if _, err := cs.CoreV1().ConfigMaps("default").Create(context.Background(), newCM("wn-probe"), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err == nil {
				return
			}
		}
	})
	probe := newCM("wn-probe")
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if _, err := cs.CoreV1().ConfigMaps("default").Create(ctx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
			return
		}
	}
	t.Fatal("the webhook never took effect")
}
