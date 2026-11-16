package crdgen

import (
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/version"
)

// plumbing#70: crdgen validates CEL for the Kubernetes version it targets, not for whatever k8s.io
// libraries it happens to link.

func ruleSchema(rule string) *apiextensions.JSONSchemaProps {
	return &apiextensions.JSONSchemaProps{Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{
		"v": {Type: "string", MaxLength: ptrInt64(64), XValidations: apiextensions.ValidationRules{{Rule: rule}}},
	}}
}

func ptrInt64(v int64) *int64 { return &v }

// The mechanism, at a target below what the libraries know: semver arrived with Kubernetes 1.33's
// CEL, so a 1.33 apiserver (compatibility 1.32) refuses it, and the error says 1.34 accepts it. The
// control, a rule using only base CEL, passes at the same target, so the refusal is about the
// function and not the instrument.
func TestCELFloorRefusesAFunctionNewerThanTheTarget(t *testing.T) {
	target := celTarget{kube: version.MajorMinor(1, 33)}
	root := field.NewPath("spec", "validation", "openAPIV3Schema")

	errs := celFloorErrors(ruleSchema("isSemver(self)"), root, target)
	if len(errs) != 1 {
		t.Fatalf("want one refusal, got %v", errs)
	}
	msg := errs[0].Error()
	for _, want := range []string{
		"spec.validation.openAPIV3Schema.properties[v].x-kubernetes-validations[0].rule",
		"a Kubernetes 1.33 apiserver refuses this",
		"it is accepted from Kubernetes 1.34",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q: %s", want, msg)
		}
	}

	if errs := celFloorErrors(ruleSchema("self != ''"), root, target); len(errs) != 0 {
		t.Errorf("a base-CEL rule must pass at any target: %v", errs)
	}
}

// Nested positions are walked the way the apiserver walks them: items and additionalProperties
// carry their own rules.
func TestCELFloorWalksItemsAndAdditionalProperties(t *testing.T) {
	target := celTarget{kube: version.MajorMinor(1, 33)}
	item := apiextensions.JSONSchemaProps{Type: "string", MaxLength: ptrInt64(64), XValidations: apiextensions.ValidationRules{{Rule: "isSemver(self)"}}}
	s := &apiextensions.JSONSchemaProps{Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{
		"list": {Type: "array", MaxItems: ptrInt64(8), Items: &apiextensions.JSONSchemaPropsOrArray{Schema: &item}},
		"map":  {Type: "object", MaxProperties: ptrInt64(8), AdditionalProperties: &apiextensions.JSONSchemaPropsOrBool{Allows: true, Schema: &item}},
	}}
	errs := celFloorErrors(s, field.NewPath("root"), target)
	var paths []string
	for _, e := range errs {
		paths = append(paths, e.Field)
	}
	got := strings.Join(paths, "|")
	want := "root.properties[list].items.x-kubernetes-validations[0].rule|root.properties[map].additionalProperties.x-kubernetes-validations[0].rule"
	if got != want {
		t.Errorf("refused paths = %s, want %s", got, want)
	}
}

// At the default target a rule using any function a 1.36 apiserver accepts generates, end to end.
func TestDefaultTargetAcceptsWhatA136ApiserverAccepts(t *testing.T) {
	for _, rule := range []string{
		"isSemver(self)", // 1.33
		"format.dns1123Label().validate(self).hasValue() || true", // 1.31
		"self.split('.').all(i, p, p != '' || i >= 0)",            // two-variable comprehension, 1.32
	} {
		t.Run(rule, func(t *testing.T) {
			schema := `{"type":"object","properties":{"v":{"type":"string","maxLength":64,"x-kubernetes-validations":[{"rule":` + jsonString(rule) + `}]}}}`
			if _, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(schema)}); err != nil {
				t.Fatalf("a 1.36 apiserver accepts this rule; generation refused it: %v", err)
			}
		})
	}
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func TestResolveCELTarget(t *testing.T) {
	for _, tc := range []struct{ in, want, err string }{
		{in: "", want: MinKubernetesVersion},
		{in: "1.36", want: "1.36"},
		{in: "v1.36.4", want: "1.36"},
		{in: "1.35", err: "below crdgen's floor 1.36: it can be raised, not lowered"},
		{in: "1.99", err: "cannot validate CEL for Kubernetes 1.99"},
		{in: "latest", err: `Options.MinKubernetesVersion "latest"`},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := resolveCELTarget(tc.in)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("want an error containing %q, got %v", tc.err, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s := got.kube.String(); s != tc.want {
				t.Errorf("target %s, want %s", s, tc.want)
			}
		})
	}
	// Through Generate: a refused target refuses the generation, before any work.
	if _, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe",
		SpecSchema: []byte(`{"type":"object"}`), MinKubernetesVersion: "1.20"}); err == nil {
		t.Error("a lowered MinKubernetesVersion must refuse generation")
	}
}

// The instrument librariesCover relies on: two compatibility versions whose CEL differs must compare
// different (1.32 lacks semver, which 1.33 adds), and a version must equal itself.
func TestCELSurfaceDistinguishesVersions(t *testing.T) {
	if sameCELSurface(version.MajorMinor(1, 32), version.MajorMinor(1, 33)) {
		t.Fatal("1.32 and 1.33 compare equal: the surface cannot see semver, so the libraries guard is blind")
	}
	if !sameCELSurface(version.MajorMinor(1, 33), version.MajorMinor(1, 33)) {
		t.Fatal("a version differs from itself")
	}
	// Macros are part of the surface: 1.32's two-variable comprehensions are macros (all(i, v, ...)),
	// which a function-only comparison would not see. No version pair differs ONLY in macros today,
	// so this asserts the keys directly.
	old, cur := celSurface(version.MajorMinor(1, 31)), celSurface(version.MajorMinor(1, 32))
	newMacro := false
	for k := range cur {
		if strings.HasPrefix(k, "macro:") && !old[k] {
			newMacro = true
		}
	}
	if !newMacro {
		t.Error("the surface does not record the macros 1.32 adds")
	}
}

// The libraries guard, with the linked versions put behind the target: libraries of 1.33 validate
// at compatibility 1.32, which lacks semver, so a 1.34 target (compatibility 1.33, which has it)
// would have crdgen refuse rules the target accepts. The guard refuses the target instead. The
// control: libraries of 1.34 validating at 1.33 cover the same target.
func TestLibrariesGuardRefusesATargetTheValidationCannotMatch(t *testing.T) {
	target := celTarget{kube: version.MajorMinor(1, 34)}
	err := target.librariesCoverWith(version.MajorMinor(1, 33), version.MajorMinor(1, 32))
	if err == nil || !strings.Contains(err.Error(), "lacks CEL functions a 1.34 apiserver accepts") {
		t.Fatalf("want the compatibility mismatch refused, got %v", err)
	}
	if err := target.librariesCoverWith(version.MajorMinor(1, 34), version.MajorMinor(1, 33)); err != nil {
		t.Fatalf("matching libraries must cover the target: %v", err)
	}
	if err := target.librariesCoverWith(version.MajorMinor(1, 32), version.MajorMinor(1, 31)); err == nil ||
		!strings.Contains(err.Error(), "do not know the CEL functions") {
		t.Fatalf("libraries older than the target's compatibility must be refused, got %v", err)
	}
}

// validateCRD applies the target: the same generated CRD, carrying a semver rule, passes at the
// default target and is refused at 1.33.
func TestValidateCRDAppliesTheTarget(t *testing.T) {
	crd := generateV1(t, `{"type":"object","properties":{"v":{"type":"string","maxLength":64,"x-kubernetes-validations":[{"rule":"isSemver(self)"}]}}}`)
	def, err := resolveCELTarget("")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCRD(crd, def); err != nil {
		t.Fatalf("default target refused a rule a 1.36 apiserver accepts: %v", err)
	}
	err = validateCRD(crd, celTarget{kube: version.MajorMinor(1, 33)})
	if err == nil || !strings.Contains(err.Error(), "a Kubernetes 1.33 apiserver refuses this") {
		t.Fatalf("want the 1.33 target to refuse the semver rule, got %v", err)
	}
}
