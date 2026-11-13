//go:build envtest

package helm

import (
	"context"
	"os"
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// plumbing#155, as osh-049a hit it, on a real apiserver: a release installed while only v1 of a
// child's CRD is served stores the child at v1. Once v2 is served the SAME chart with the SAME values
// renders the child at v2 (it picks the newest served version through .Capabilities, as the
// blueprint umbrella does). One Reconcile must write a revision whose stored manifest names v2;
// a second Reconcile, with nothing left to move, must write none (the control).
func TestEnvtest_ReconcileAdvancesAVersionOnlyRenderChange(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ac := apiextclient.NewForConfigOrDie(cfg)
	ctx := context.Background()

	version := func(name string, storage bool) apixv1.CustomResourceDefinitionVersion {
		return apixv1.CustomResourceDefinitionVersion{Name: name, Served: true, Storage: storage,
			Schema: &apixv1.CustomResourceValidation{OpenAPIV3Schema: &apixv1.JSONSchemaProps{Type: "object",
				Properties: map[string]apixv1.JSONSchemaProps{"spec": {Type: "object", XPreserveUnknownFields: boolp(true)}}}}}
	}
	crd := &apixv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "sprockets.composition.krateo.io"},
		Spec: apixv1.CustomResourceDefinitionSpec{Group: "composition.krateo.io", Scope: apixv1.NamespaceScoped,
			Names:    apixv1.CustomResourceDefinitionNames{Plural: "sprockets", Singular: "sprocket", Kind: "Sprocket", ListKind: "SprocketList"},
			Versions: []apixv1.CustomResourceDefinitionVersion{version("v1", true)}},
	}
	if _, err := ac.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEstablished(t, ac, crd.Name)

	// A fresh client per phase, so discovery reflects the served versions at that moment: the
	// question here is what Reconcile does with a changed render, not discovery caching.
	newClient := func() *client {
		t.Helper()
		hc, err := NewClient(cfg, WithNamespace("default"))
		if err != nil {
			t.Fatal(err)
		}
		return hc
	}
	url := serveFiles(t, map[string]string{
		"Chart.yaml": chartYAML,
		"templates/s.yaml": `{{- $v := "v1" }}{{- if .Capabilities.APIVersions.Has "composition.krateo.io/v2/Sprocket" }}{{- $v = "v2" }}{{- end }}
apiVersion: composition.krateo.io/{{ $v }}
kind: Sprocket
metadata:
  name: s
  namespace: default
spec:
  k: v
`,
	})
	if _, err := newClient().Install(ctx, "stale", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	storedManifest := func(hc *client) (string, int) {
		t.Helper()
		rel, err := hc.GetRelease(ctx, "stale", &helmconfig.GetConfig{})
		if err != nil || rel == nil {
			t.Fatalf("get release: %v", err)
		}
		return rel.Manifest, rel.Revision
	}
	if m, _ := storedManifest(newClient()); !strings.Contains(m, "composition.krateo.io/v1") {
		t.Fatalf("precondition: the install stored the child at v1; manifest:\n%s", m)
	}

	// v2 becomes served (v1 stays the storage version, so no conversion is involved).
	cur, err := ac.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Spec.Versions = append(cur.Spec.Versions, version("v2", false))
	if _, err := ac.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEstablished(t, ac, crd.Name)

	hc := newClient()
	// Precondition: the render now emits v2. Without it a failure below would be about discovery,
	// not about Reconcile.
	dry, err := hc.Upgrade(ctx, "stale", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{DryRun: helmconfig.DryRunServer}})
	if err != nil || dry == nil || dry.Release == nil || !strings.Contains(dry.Release.Manifest, "composition.krateo.io/v2") {
		t.Fatalf("precondition: the render emits the child at v2 once v2 is served (err %v)", err)
	}
	_, before := storedManifest(hc)

	res, err := hc.Reconcile(ctx, "stale", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	m, after := storedManifest(hc)
	if !res.Changed || after != before+1 || !strings.Contains(m, "composition.krateo.io/v2") || strings.Contains(m, "composition.krateo.io/v1") {
		t.Fatalf("a version-only render change: Changed %v, revision %d -> %d, stale %q; stored manifest:\n%s\nwant Changed, one new revision, the manifest naming v2 only",
			res.Changed, before, after, res.StaleManifestVersions, m)
	}
	if len(res.StaleManifestVersions) != 1 || !strings.Contains(res.StaleManifestVersions[0], "composition.krateo.io/v1 -> composition.krateo.io/v2") {
		t.Errorf("StaleManifestVersions = %q, want the one child v1 -> v2", res.StaleManifestVersions)
	}
	for _, v := range res.Objects {
		// An unchanged child has no verdict; above all it must not read as a content change, which
		// a consumer records (the cdc's audit).
		if v.Name == "s" && (v.Change == helmconfig.ChangeSpec || v.Change == helmconfig.ChangeCreated) {
			t.Errorf("the child's own verdict = %q, want none (its content did not change)", v.Change)
		}
	}

	// Control: nothing left to move, no revision.
	res, err = newClient().Reconcile(ctx, "stale", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{}})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if _, again := storedManifest(newClient()); res.Changed || again != after || len(res.StaleManifestVersions) != 0 {
		t.Errorf("control: a second reconcile Changed %v, revision %d -> %d, stale %q; want no change", res.Changed, after, again, res.StaleManifestVersions)
	}
}
