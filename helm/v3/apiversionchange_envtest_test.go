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
	"sync/atomic"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// installer asked (core-provider#533's writer fix): when an umbrella re-renders a release so one of
// its custom resources moves from apiVersion v1 to v2, same group, kind, name and namespace, does helm
// (or plumbing's Reconcile) treat it as a DIFFERENT object, deleting and recreating it? If it did,
// the two-pass fix would recreate every composition. From code it should not: helm matches release
// objects by name+namespace+kind+group with no version (isMatchingInfo), and plumbing's deletion set
// uses helm's own Difference. This pins it on a real apiserver, on both of Reconcile's paths.

var probeGVR1 = schema.GroupVersionResource{Group: "example.krateo.io", Version: "v1", Resource: "probes"}

func twoVersionCRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	sch := &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"spec": {Type: "object", XPreserveUnknownFields: &preserve},
		},
	}}
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "probes.example.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.krateo.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "probes", Singular: "probe", Kind: "Probe", ListKind: "ProbeList"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true, Storage: true, Schema: sch},
				{Name: "v2", Served: true, Storage: false, Schema: sch},
			},
		},
	}
}

// swappableChart serves whatever archive was last set, at one fixed URL and chart version, so a
// test can change what the SAME chart version renders (what an umbrella's `lookup` does between
// passes) without changing Reconcile's inputs.
type swappableChart struct{ tgz atomic.Value }

func (s *swappableChart) set(t *testing.T, files map[string]string) {
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
	s.tgz.Store(buf.Bytes())
}

func probeTemplate(apiVersion string) string {
	// The `rendered` label is the WITNESS that the render actually changed: only the v2 archive
	// carries rendered=v2, so finding it live proves the v2 manifest was applied, not a cached v1.
	return "apiVersion: example.krateo.io/" + apiVersion + "\nkind: Probe\nmetadata:\n  name: p\n  namespace: default\n  labels:\n    rendered: " + apiVersion + "\nspec:\n  value: {{ .Values.k | quote }}\n"
}

func TestEnvtest_ApiVersionChangeKeepsTheObject(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{twoVersionCRD()}}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx := context.Background()

	newClient := func(t *testing.T) *client {
		// No chart cache: the merge case changes what one URL serves, and a cached archive would
		// hide that.
		hc, err := NewClient(cfg, WithNamespace("default"))
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		t.Cleanup(func() { _ = hc.Close() })
		return hc
	}
	uidOf := func(t *testing.T) (string, string) {
		t.Helper()
		u, err := dyn.Resource(probeGVR1).Namespace("default").Get(ctx, "p", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("read the probe: %v", err)
		}
		v, _, _ := unstructuredString(u.Object, "spec", "value")
		return string(u.GetUID()), v
	}
	cleanup := func(t *testing.T, hc *client, release string) {
		t.Cleanup(func() {
			_, _ = hc.Uninstall(context.Background(), release, &helmconfig.UninstallConfig{})
			_ = dyn.Resource(probeGVR1).Namespace("default").Delete(context.Background(), "p", metav1.DeleteOptions{})
		})
	}

	// installer's pass 2: the version AND the values move, so #182 sends it through a real upgrade.
	t.Run("upgrade path: values and apiVersion change together", func(t *testing.T) {
		hc := newClient(t)
		var sc swappableChart
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sc.tgz.Load().([]byte)) }))
		t.Cleanup(srv.Close)
		url := srv.URL + "/c-0.1.0.tgz"
		sc.set(t, map[string]string{"Chart.yaml": chartYAML, "templates/p.yaml": "apiVersion: example.krateo.io/{{ .Values.ver }}\nkind: Probe\nmetadata:\n  name: p\n  namespace: default\nspec:\n  value: {{ .Values.k | quote }}\n"})
		cleanup(t, hc, "verup")

		if _, err := hc.Install(ctx, "verup", url, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"ver": "v1", "k": "a"}}}); err != nil {
			t.Fatalf("install at v1: %v", err)
		}
		uid0, _ := uidOf(t)
		res, err := hc.Reconcile(ctx, "verup", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"ver": "v2", "k": "b"}}})
		if err != nil {
			t.Fatalf("reconcile at v2: %v", err)
		}
		uid1, val := uidOf(t)
		if uid1 != uid0 {
			t.Fatalf("the object was DELETED AND RECREATED by an apiVersion change: uid %s -> %s", uid0, uid1)
		}
		if val != "b" {
			t.Errorf("the new values did not land: spec.value=%q, want b", val)
		}
		if res == nil || res.Release == nil || res.Release.Revision != 2 {
			t.Errorf("want a recorded revision 2 for the change, got %+v", res)
		}
	})

	// The same chart version and values, but the render moves to v2: what an umbrella's `lookup`
	// does between passes. Reconcile sees no desired-state change and heals through helm's
	// three-way merge, whose RequireKeyUnchanged("apiVersion") precondition is the part to prove.
	t.Run("merge path: only the rendered apiVersion changes", func(t *testing.T) {
		hc := newClient(t)
		var sc swappableChart
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sc.tgz.Load().([]byte)) }))
		t.Cleanup(srv.Close)
		url := srv.URL + "/c-0.1.0.tgz"
		sc.set(t, map[string]string{"Chart.yaml": chartYAML, "templates/p.yaml": probeTemplate("v1")})
		cleanup(t, hc, "vermerge")

		vals := map[string]any{"k": "a"}
		if _, err := hc.Install(ctx, "vermerge", url, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: &helmconfig.ActionConfig{Values: vals}}); err != nil {
			t.Fatalf("install at v1: %v", err)
		}
		uid0, _ := uidOf(t)
		sc.set(t, map[string]string{"Chart.yaml": chartYAML, "templates/p.yaml": probeTemplate("v2")})
		res, err := hc.Reconcile(ctx, "vermerge", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: vals}})
		if err != nil {
			t.Fatalf("reconcile with the render moved to v2 refused: %v", err)
		}
		if res != nil && res.DesiredStateChange != "" {
			t.Fatalf("this case must exercise the MERGE path, but Reconcile saw a desired-state change %q", res.DesiredStateChange)
		}
		uid1, val := uidOf(t)
		if uid1 != uid0 {
			t.Fatalf("the object was DELETED AND RECREATED by an apiVersion change: uid %s -> %s", uid0, uid1)
		}
		if val != "a" {
			t.Errorf("spec.value changed to %q, want a", val)
		}
		live, err := dyn.Resource(probeGVR1).Namespace("default").Get(ctx, "p", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := live.GetLabels()["rendered"]; got != "v2" {
			t.Fatalf("the v2 render never reached the object (label rendered=%q): this case did not exercise an apiVersion change", got)
		}
	})

	// Control: the UID comparison above can fire. A real delete-and-recreate (the object removed
	// out of band, Reconcile putting it back) must show a DIFFERENT uid, or "same uid" proves nothing.
	t.Run("control: a real recreate changes the uid", func(t *testing.T) {
		hc := newClient(t)
		var sc swappableChart
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sc.tgz.Load().([]byte)) }))
		t.Cleanup(srv.Close)
		url := srv.URL + "/c-0.1.0.tgz"
		sc.set(t, map[string]string{"Chart.yaml": chartYAML, "templates/p.yaml": probeTemplate("v1")})
		cleanup(t, hc, "verctl")
		vals := map[string]any{"k": "a"}
		if _, err := hc.Install(ctx, "verctl", url, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: &helmconfig.ActionConfig{Values: vals}}); err != nil {
			t.Fatalf("install: %v", err)
		}
		uid0, _ := uidOf(t)
		if err := dyn.Resource(probeGVR1).Namespace("default").Delete(ctx, "p", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := hc.Reconcile(ctx, "verctl", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: vals}}); err != nil {
			t.Fatalf("reconcile after an out-of-band delete: %v", err)
		}
		if uid1, _ := uidOf(t); uid1 == uid0 {
			t.Fatalf("a recreated object kept uid %s: the uid comparison cannot detect a recreate", uid0)
		}
	})
}

func unstructuredString(obj map[string]any, path ...string) (string, bool, error) {
	cur := any(obj)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur = m[p]
	}
	s, ok := cur.(string)
	return s, ok, nil
}
