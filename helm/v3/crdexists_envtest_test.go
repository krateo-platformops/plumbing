//go:build envtest

// Functional (real-apiserver) test of the cache-staleness surface. Unlike the fake-discovery unit
// tests, this stands up a real kube-apiserver via controller-runtime/envtest, renders the ACTUAL
// installer gating mechanism (inst.crdExists = a helm `lookup` of every CustomResourceDefinition,
// ranged in-memory), registers a CRD MID-RUN, and re-renders on the SAME long-lived helm client to
// answer the load-bearing question empirically: does the render pick up a newly-registered CRD
// WITHOUT a process restart, and does helm.WithCRDInformer change that outcome?
//
// Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) go test -tags envtest ./helm/v3/ -run Envtest -v
package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

func boolp(b bool) *bool { return &b }

// widgetCRD is a composition.krateo.io/v1-0-0 Widget CRD — the shape inst.crdExists matches on
// (spec.group == composition.krateo.io, spec.names.kind, a served version "v1-0-0").
func widgetCRD() *apixv1.CustomResourceDefinition {
	return &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.composition.krateo.io"},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apixv1.CustomResourceDefinitionNames{
				Plural:   "widgets",
				Singular: "widget",
				Kind:     "Widget",
				ListKind: "WidgetList",
			},
			Scope: apixv1.NamespaceScoped,
			Versions: []apixv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1-0-0",
					Served:  true,
					Storage: true,
					Schema: &apixv1.CustomResourceValidation{
						OpenAPIV3Schema: &apixv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apixv1.JSONSchemaProps{
								"spec": {Type: "object", XPreserveUnknownFields: boolp(true)},
							},
						},
					},
				},
			},
		},
	}
}

// serveCRDProbeChart packages a tiny chart (a gzipped tar, served over HTTP at a .tgz URL — the form
// the plumbing getter accepts) whose single template reproduces the inst.crdExists logic: list every
// CustomResourceDefinition (a built-in GVK, so its own discovery is never stale) and range it looking
// for the composition.krateo.io Widget kind at served version v1-0-0. Returns the chart .tgz URL.
func serveCRDProbeChart(t *testing.T) string {
	t.Helper()
	files := []struct{ name, body string }{
		{"crdprobe/Chart.yaml", "apiVersion: v2\nname: crdprobe\nversion: 0.1.0\n"},
		{"crdprobe/templates/probe.yaml", `{{- $crds := (lookup "apiextensions.k8s.io/v1" "CustomResourceDefinition" "" "").items -}}
{{- $found := "" -}}
{{- range $crds -}}
{{- if and (eq .spec.group "composition.krateo.io") (eq .spec.names.kind "Widget") -}}
{{- range .spec.versions -}}{{- if and (eq .name "v1-0-0") .served -}}{{- $found = "true" -}}{{- end -}}{{- end -}}
{{- end -}}
{{- end -}}
apiVersion: v1
kind: ConfigMap
metadata:
  name: crdprobe-result
data:
  widgetCRDSeen: "{{ $found }}"
  crdCount: "{{ len $crds }}"
`},
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
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	tgz := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/crdprobe-0.1.0.tgz"
}

// widgetSeen extracts the widgetCRDSeen value from a rendered manifest ("" = CRD not seen).
func widgetSeen(manifest string) string {
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "widgetCRDSeen:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "widgetCRDSeen:")), `"`)
		}
	}
	return "<not-rendered>"
}

// renderProbe runs a DryRunServer install (executes `lookup` against the live apiserver, persists
// nothing) on the given client and returns the rendered manifest.
func renderProbe(t *testing.T, hc *client, chartURL, releaseName string) string {
	t.Helper()
	rel, err := releaseOf(hc.Install(context.Background(), releaseName, chartURL, &helmconfig.InstallConfig{
		ActionConfig: &helmconfig.ActionConfig{DryRun: helmconfig.DryRunServer},
		Namespace:    "default",
	}))
	if err != nil {
		t.Fatalf("render (%s): %v", releaseName, err)
	}
	return rel.GetManifest()
}

func waitEstablished(t *testing.T, ac apiextclient.Interface, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		crd, err := ac.ApiextensionsV1().CustomResourceDefinitions().Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			for _, c := range crd.Status.Conditions {
				if c.Type == apixv1.Established && c.Status == apixv1.ConditionTrue {
					return
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("CRD %s never became Established", name)
}

// TestEnvtest_CRDExistsRender_PicksUpMidRunCRD is the diagnostic: it registers the Widget CRD while a
// long-lived helm client is alive and asserts the SAME client's next crdExists render sees it — no
// restart. It runs the assertion for BOTH the cdc's current construction (WithCache only) and the
// proposed fix (WithCRDInformer), logging the empirical outcome of each so the fix's necessity/location
// is grounded in observed behavior, not assumption.
func TestEnvtest_CRDExistsRender_PicksUpMidRunCRD(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()

	ac, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("apiextensions client: %v", err)
	}
	chartURL := serveCRDProbeChart(t)

	// ---- cdc-current construction: WithCache only (cachedClients stays nil) ----
	cur, err := NewClient(cfg, WithNamespace("default"), WithCache())
	if err != nil {
		t.Fatalf("new WithCache client: %v", err)
	}
	defer func() { _ = cur.Close() }()
	t.Logf("cdc-current client cachedClients==nil: %v", cur.cachedClients == nil)

	if got := widgetSeen(renderProbe(t, cur, chartURL, "probe-cur-1")); got != "" {
		t.Fatalf("precondition: Widget CRD should be absent, render reported widgetCRDSeen=%q", got)
	}

	// Register the CRD MID-RUN — no client restart, no Close.
	if _, err := ac.ApiextensionsV1().CustomResourceDefinitions().Create(context.Background(), widgetCRD(), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create Widget CRD: %v", err)
	}
	waitEstablished(t, ac, "widgets.composition.krateo.io")

	seenCur := widgetSeen(renderProbe(t, cur, chartURL, "probe-cur-2"))
	t.Logf("EMPIRICAL [WithCache only]: same client crdExists render after mid-run CRD => widgetCRDSeen=%q", seenCur)

	// ---- cdc-fixed construction: WithCRDInformer (shared cachedClients + informer invalidation) ----
	fix, err := NewClient(cfg, WithNamespace("default"), WithCache(), WithCRDInformer(cfg, time.Minute))
	if err != nil {
		t.Fatalf("new WithCRDInformer client: %v", err)
	}
	defer func() { _ = fix.Close() }()
	t.Logf("cdc-fixed client cachedClients==nil: %v", fix.cachedClients == nil)

	seenFix := widgetSeen(renderProbe(t, fix, chartURL, "probe-fix-1"))
	t.Logf("EMPIRICAL [WithCRDInformer]: crdExists render (CRD already present) => widgetCRDSeen=%q", seenFix)

	// The load-bearing claim the user asked to verify: crdExists sees a mid-run CRD without a restart.
	// The crdExists path lists a BUILT-IN GVK (CustomResourceDefinition), so both constructions are
	// expected to see it; this assertion pins that and will surface it loudly if reality differs.
	if seenCur != "true" {
		t.Errorf("REGRESSION/DIAGNOSTIC: WithCache-only client did NOT see the mid-run CRD in crdExists render "+
			"(widgetCRDSeen=%q); if this fails, crdExists IS stale at the helm layer", seenCur)
	}
	if seenFix != "true" {
		t.Errorf("WithCRDInformer client did not see the present CRD (widgetCRDSeen=%q)", seenFix)
	}
}
