//go:build envtest

package helm

import (
	"context"
	"errors"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// plumbing#72: NoHooks, SkipCRDs and SkipSchemaValidation were declared on ActionConfig and read
// nowhere, so hooks still ran, CRDs were still installed and values were still validated, with no
// error. Each switch is proven against a real apiserver both ways: on, the effect is absent; off
// (the control), it is present, so a passing "on" arm cannot be a chart that never had the effect.

const releaseNamedPostInstallHook = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}-hook
  namespace: default
  annotations:
    helm.sh/hook: post-install,pre-upgrade
data:
  k: v
`

func cmExists(t *testing.T, kc kubernetes.Interface, name string) bool {
	t.Helper()
	_, err := kc.CoreV1().ConfigMaps("default").Get(context.Background(), name, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get configmap %s: %v", name, err)
	}
	return false
}

func TestEnvtest_NoHooksSkipsHooksOnInstallAndUpgrade(t *testing.T) {
	hc := envtestClient(t)
	kc := kubernetes.NewForConfigOrDie(hc.restConfig)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/cm.yaml": valuedCM("{{ .Release.Name }}-app", "default"), "templates/hook.yaml": releaseNamedPostInstallHook})

	// Install.
	if _, err := hc.Install(ctx, "nohooks", url, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: &helmconfig.ActionConfig{NoHooks: true}}); err != nil {
		t.Fatalf("install with NoHooks: %v", err)
	}
	if _, err := hc.Install(ctx, "hooks", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install (control): %v", err)
	}
	if !cmExists(t, kc, "nohooks-app") || !cmExists(t, kc, "hooks-app") {
		t.Fatal("control: both releases must have installed their ordinary object")
	}
	if !cmExists(t, kc, "hooks-hook") {
		t.Fatal("control: without NoHooks the post-install hook must run, or this test sees nothing")
	}
	if cmExists(t, kc, "nohooks-hook") {
		t.Error("Install with NoHooks ran the post-install hook")
	}

	// Upgrade: delete the control's hook object first so its pre-upgrade run is visible again.
	if err := kc.CoreV1().ConfigMaps("default").Delete(ctx, "hooks-hook", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := hc.Upgrade(ctx, "nohooks", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{NoHooks: true, Values: map[string]any{"k": "b"}}}); err != nil {
		t.Fatalf("upgrade with NoHooks: %v", err)
	}
	if _, err := hc.Upgrade(ctx, "hooks", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"k": "b"}}}); err != nil {
		t.Fatalf("upgrade (control): %v", err)
	}
	if !cmExists(t, kc, "hooks-hook") {
		t.Fatal("control: without NoHooks the pre-upgrade hook must run, or this test sees nothing")
	}
	if cmExists(t, kc, "nohooks-hook") {
		t.Error("Upgrade with NoHooks ran the pre-upgrade hook")
	}
}

func crdFile(plural, group string) string {
	return `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ` + plural + `.` + group + `
spec:
  group: ` + group + `
  names: {kind: Widget, plural: ` + plural + `, singular: widget, listKind: WidgetList}
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema: {openAPIV3Schema: {type: object, x-kubernetes-preserve-unknown-fields: true}}
`
}

func TestEnvtest_SkipCRDsLeavesTheChartsCRDsUninstalled(t *testing.T) {
	hc := envtestClient(t)
	ax := apiextclient.NewForConfigOrDie(hc.restConfig)
	ctx := context.Background()
	crdExists := func(name string) bool {
		_, err := ax.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return true
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get crd %s: %v", name, err)
		}
		return false
	}
	// crds/ is not templated, so each release gets a chart of its own with its own CRD name.
	skip := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "crds/crd.yaml": crdFile("widgets", "skip.example.com"), "templates/cm.yaml": valuedCM("skipcrds-app", "default")})
	keep := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "crds/crd.yaml": crdFile("widgets", "keep.example.com"), "templates/cm.yaml": valuedCM("keepcrds-app", "default")})
	if _, err := hc.Install(ctx, "skipcrds", skip, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: &helmconfig.ActionConfig{SkipCRDs: true}}); err != nil {
		t.Fatalf("install with SkipCRDs: %v", err)
	}
	if _, err := hc.Install(ctx, "keepcrds", keep, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install (control): %v", err)
	}
	if !crdExists("widgets.keep.example.com") {
		t.Fatal("control: without SkipCRDs the chart's CRD must be installed, or this test sees nothing")
	}
	if crdExists("widgets.skip.example.com") {
		t.Error("Install with SkipCRDs installed the chart's CRD")
	}
}

const stringOnlySchema = `{"type":"object","properties":{"k":{"type":"string"}}}`

func TestEnvtest_SkipSchemaValidationSkipsBothChecks(t *testing.T) {
	hc := envtestClient(t)
	kc := kubernetes.NewForConfigOrDie(hc.restConfig)
	ctx := context.Background()
	url := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "values.schema.json": stringOnlySchema, "templates/cm.yaml": valuedCM("{{ .Release.Name }}-app", "default")})
	bad := map[string]any{"k": 5} // violates the schema: k must be a string

	// Control: without the switch the install is refused, by plumbing's own pre-check.
	_, err := hc.Install(ctx, "validated", url, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: &helmconfig.ActionConfig{Values: bad}})
	if !errors.Is(err, helmconfig.ErrValuesSchema) {
		t.Fatalf("control: a schema-violating install must fail with ErrValuesSchema, got %v", err)
	}
	// With it, neither plumbing's pre-check nor helm's render-time validation refuses it.
	if _, err := hc.Install(ctx, "unvalidated", url, &helmconfig.InstallConfig{Namespace: "default", ActionConfig: &helmconfig.ActionConfig{SkipSchemaValidation: true, Values: bad}}); err != nil {
		t.Fatalf("install with SkipSchemaValidation was refused: %v", err)
	}
	if !cmExists(t, kc, "unvalidated-app") {
		t.Error("install with SkipSchemaValidation reported success but wrote nothing")
	}

	// Upgrade, both ways.
	_, err = hc.Upgrade(ctx, "unvalidated", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: bad}})
	if !errors.Is(err, helmconfig.ErrValuesSchema) {
		t.Fatalf("control: a schema-violating upgrade must fail with ErrValuesSchema, got %v", err)
	}
	if _, err := hc.Upgrade(ctx, "unvalidated", url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{SkipSchemaValidation: true, Values: bad}}); err != nil {
		t.Fatalf("upgrade with SkipSchemaValidation was refused: %v", err)
	}
}
