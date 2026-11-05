package crdgen

import (
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// installer 0.3.508's identity-lifecycle shape: a forbidden-values constraint on the items of TWO
// unbounded arrays (identityResources[].resources[]), on an unbounded string.
const identityShape = `{"type":"object","properties":{"identityResources":{"type":"array","items":{"type":"object","properties":{"resources":{"type":"array","items":{"type":"string","minLength":1,"not":{"enum":["*","secrets"]}}}}}}}}`

func generateV1(t *testing.T, spec string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	out, warns, err := GenerateWithWarnings(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(spec)})
	if err != nil {
		t.Fatalf("generation refused: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings (a dropped constraint would show here): %v", warns)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	return &crd
}

// admit validates a CR body against the generated CRD with the apiserver's OWN schema validator.
func admit(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, obj map[string]any) field.ErrorList {
	t.Helper()
	scheme := runtime.NewScheme()
	apiextinstall.Install(scheme)
	var internal apiextensions.JSONSchemaProps
	if err := scheme.Convert(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	v, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	return validation.ValidateCustomResource(nil, obj, v)
}

func cr(resources ...string) map[string]any {
	items := make([]any, len(resources))
	for i, r := range resources {
		items[i] = r
	}
	return map[string]any{"apiVersion": "composition.krateo.io/v1", "kind": "Probe",
		"spec": map[string]any{"identityResources": []any{map[string]any{"resources": items}}}}
}

// THE REFUSAL, GONE, AND THE CONSTRAINT STILL ENFORCED. The CEL translation of this `not` was refused
// at 1.468006x over the cost budget, taking the whole CRD with it (installer 0.3.508, snowplow
// 1.12.45). The native `not` must (1) generate, (2) carry NO CEL rule, and (3) still REJECT the
// forbidden values at admission. The third is what makes this a fix and not a silent drop.
func TestNotEnumIsNativeAndEnforced(t *testing.T) {
	crd := generateV1(t, identityShape)

	if strings.Contains(mustYAML(t, crd), "x-kubernetes-validations") {
		t.Error("a CEL rule is still emitted for the forbidden-values constraint")
	}
	for _, bad := range []string{"*", "secrets"} {
		if errs := admit(t, crd, cr("pods", bad)); len(errs) == 0 {
			t.Errorf("resources containing %q was ADMITTED: the security constraint is no longer enforced", bad)
		}
	}
	// CONTROL: allowed values pass, or the arm above passes against a schema that rejects everything.
	if errs := admit(t, crd, cr("pods", "configmaps")); len(errs) != 0 {
		t.Errorf("allowed values were rejected: %v", errs)
	}
}

// const is the one-value case of the same translation.
func TestNotConstIsNativeAndEnforced(t *testing.T) {
	crd := generateV1(t, `{"type":"object","properties":{"region":{"type":"string","not":{"const":"banned"}}}}`)
	obj := func(r string) map[string]any {
		return map[string]any{"apiVersion": "composition.krateo.io/v1", "kind": "Probe", "spec": map[string]any{"region": r}}
	}
	if errs := admit(t, crd, obj("banned")); len(errs) == 0 {
		t.Error(`region "banned" was admitted`)
	}
	if errs := admit(t, crd, obj("eu")); len(errs) != 0 {
		t.Errorf(`region "eu" was rejected: %v`, errs)
	}
}

func mustYAML(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) string {
	t.Helper()
	b, err := yaml.Marshal(crd)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
