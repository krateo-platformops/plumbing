//go:build envtest

// Functional (real-apiserver) proof that an Upgrade on a long-lived, shared-discovery client picks
// up a CRD VERSION REGISTERED AFTER the client has already rendered successfully — the ordering that
// IS the bug, and which no fixture that registers the CRD up front can reproduce.
//
// The unit tests next door prove the wrapper retries (behaviour) and that every resolving operation
// calls it (mention). Neither proves the whole chain against a real apiserver, and the consumer who
// found the outage asked specifically for this ordering, because:
//
//	"A test that only creates the CRD before the client exists will pass on the broken code."
//
// Sequence, matching the production failure exactly:
//
//  1. build ONE client with WithCachedClients  (process-lifetime DeferredDiscoveryRESTMapper)
//  2. Upgrade once against a chart that needs only built-in kinds  -> warms discovery
//  3. register a NEW CRD + version                                 -> the cluster now serves it
//  4. Upgrade again against a chart that references that kind
//     broken: `no matches for kind ... ensure CRDs are installed first`, forever
//     fixed:  the mapping miss resets discovery and the retry renders it
//
// Run: KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.36.0) go test -tags envtest ./helm/v3/ -run Envtest_Upgrade -v
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

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// serveKindChart serves a one-resource chart over HTTP whose single template declares the given
// kind/apiVersion. helm must REST-map that kind to render it, which is what makes the chart a probe
// for discovery staleness rather than for template correctness.
func serveKindChart(t *testing.T, kind, apiVersion string) string {
	t.Helper()
	files := []struct{ name, body string }{
		{"kindprobe/Chart.yaml", "apiVersion: v2\nname: kindprobe\nversion: 0.1.0\n"},
		{"kindprobe/templates/obj.yaml", "apiVersion: " + apiVersion + "\nkind: " + kind +
			"\nmetadata:\n  name: kindprobe-" + strings.ToLower(kind) + "\n  namespace: default\n"},
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/kindprobe-0.1.0.tgz"
}

// gadgetCRD is a second kind, distinct from widgetCRD, so this test's mid-run registration cannot
// be satisfied by anything the CRDExists test already put in the cluster.
func gadgetCRD() *apixv1.CustomResourceDefinition {
	return &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "gadgets.composition.krateo.io"},
		Spec: apixv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apixv1.CustomResourceDefinitionNames{
				Plural: "gadgets", Singular: "gadget", Kind: "Gadget", ListKind: "GadgetList",
			},
			Scope: apixv1.NamespaceScoped,
			Versions: []apixv1.CustomResourceDefinitionVersion{{
				Name: "v0-1-1", Served: true, Storage: true,
				Schema: &apixv1.CustomResourceValidation{
					OpenAPIV3Schema: &apixv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apixv1.JSONSchemaProps{
							"spec": {Type: "object", XPreserveUnknownFields: boolp(true)},
						},
					},
				},
			}},
		},
	}
}

func TestEnvtest_UpgradePicksUpCRDVersionRegisteredAfterWarmup(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	defer func() { _ = env.Stop() }()

	ac, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("apiext client: %v", err)
	}

	// ONE process-wide CachedClients, shared exactly as a long-running controller shares it. This
	// is the object whose mapper outlives every operation and therefore goes stale.
	cc, err := NewCachedClients(cfg)
	if err != nil {
		t.Fatalf("NewCachedClients: %v", err)
	}
	hc, err := NewClient(cfg, WithNamespace("default"), WithCachedClients(&cc))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c := hc

	// STEP 2 — warm discovery with a successful render that needs only built-in kinds. Without
	// this the mapper would be built AFTER the CRD exists and the bug would not reproduce; this
	// line is the whole reason the test is ordered rather than arranged.
	// A REAL install, so the release exists and later Upgrades of it take the upgrade path rather
	// than falling back to install — which is the shape production has: the umbrella's release
	// exists and is upgraded on every reconcile.
	warmChart := serveKindChart(t, "ConfigMap", "v1")
	if _, err := c.Install(context.Background(), "probe", warmChart, &helmconfig.InstallConfig{
		ActionConfig: &helmconfig.ActionConfig{},
		Namespace:    "default",
	}); err != nil {
		t.Fatalf("warmup install (built-in kinds only) must succeed: %v", err)
	}

	// CONTROL — the kind must NOT resolve yet, and must fail in the way the retry classifies. If
	// this passes, discovery was not actually warmed and the rest of the test proves nothing.
	gadgetChart := serveKindChart(t, "Gadget", "composition.krateo.io/v0-1-1")
	_, preErr := c.Upgrade(context.Background(), "probe", gadgetChart, &helmconfig.UpgradeConfig{
		ActionConfig: &helmconfig.ActionConfig{DryRun: helmconfig.DryRunServer},
	})
	if preErr == nil {
		t.Fatal("control: Gadget resolved before its CRD existed — the cluster or the fixture is wrong")
	}
	if !IsRESTMappingMiss(preErr) {
		t.Fatalf("control: pre-registration failure should be a mapping miss, got: %v", preErr)
	}

	// STEP 3 — the CRD lands mid-run, as it does when a render registers the next level's
	// CompositionDefinition.
	if _, err := ac.ApiextensionsV1().CustomResourceDefinitions().
		Create(context.Background(), gadgetCRD(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating Gadget CRD: %v", err)
	}
	waitEstablished(t, ac, "gadgets.composition.krateo.io")

	// STEP 4 — the same client, no restart. On the broken code this fails identically to the
	// control, forever; the production symptom was exactly this, repeating every reconcile.
	rel, err := c.Upgrade(context.Background(), "probe", gadgetChart, &helmconfig.UpgradeConfig{
		ActionConfig: &helmconfig.ActionConfig{DryRun: helmconfig.DryRunServer},
	})
	if err != nil {
		t.Fatalf("Upgrade did not recover after the CRD version was registered — a stale "+
			"process-lifetime mapper still wedges the render, which is the whole defect: %v", err)
	}
	if !strings.Contains(rel.GetManifest(), "kind: Gadget") {
		t.Errorf("render succeeded but did not contain the Gadget resource; manifest:\n%s", rel.GetManifest())
	}
}
