package crdgen

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// plumbing#48: a `not` over one level of allOf/anyOf/oneOf of value validations is emitted natively
// and enforced. It used to be dropped with a warning, so "refuse * and every system: group" left
// the CRD admitting exactly those values. Judged by the apiserver's own validator; generateV1 also
// fails on any warning, so a drop shows here too.
func TestNotOverAJunctionIsNativeAndEnforced(t *testing.T) {
	for _, tc := range []struct {
		name              string
		schema            string
		refused, admitted []string
	}{
		{"the issue's repro: not anyOf [enum, pattern] on array items",
			`{"type":"array","maxItems":8,"items":{"type":"string","not":{"anyOf":[{"enum":["*"]},{"pattern":"^system:"}]}}}`,
			[]string{"*", "system:masters", "system:"}, []string{"dev", "systemx", "a*"}},
		{"not allOf",
			`{"type":"string","not":{"allOf":[{"minLength":3},{"pattern":"^a"}]}}`,
			[]string{"abc", "abcd"}, []string{"ab", "xyz", "a"}},
		// oneOf is exactly one: "ab" matches both members, so oneOf is false and not admits it.
		{"not oneOf",
			`{"type":"string","not":{"oneOf":[{"pattern":"a"},{"pattern":"b"}]}}`,
			[]string{"a", "b", "xa"}, []string{"ab", "c"}},
		// A member restating the field's own type is carried without it, losslessly.
		{"a member of the field's own type",
			`{"type":"string","not":{"anyOf":[{"type":"string","enum":["x"]},{"const":"y"}]}}`,
			[]string{"x", "y"}, []string{"z"}},
		{"value validations beside the junction",
			`{"type":"string","not":{"pattern":"^p","anyOf":[{"minLength":3},{"enum":["pq"]}]}}`,
			[]string{"pqr", "pq"}, []string{"p", "xyz"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crd := generateV1(t, `{"type":"object","properties":{"f":`+tc.schema+`}}`)
			if errs := structuralErrs(t, crd); len(errs) != 0 {
				t.Fatalf("the apiserver would refuse this CRD: %v", errs)
			}
			if y := mustYAML(t, crd); strings.Contains(y, "x-kubernetes-validations") {
				t.Error("a CEL rule was emitted; the negated junction must be a native not")
			}
			wrap := func(v string) any {
				if strings.HasPrefix(tc.schema, `{"type":"array"`) {
					return []any{v}
				}
				return v
			}
			for _, v := range tc.refused {
				if errs := admit(t, crd, probe("f", wrap(v))); len(errs) == 0 {
					t.Errorf("%q was ADMITTED: the negated junction is not enforced", v)
				}
			}
			for _, v := range tc.admitted {
				if errs := admit(t, crd, probe("f", wrap(v))); len(errs) != 0 {
					t.Errorf("%q was refused: %v", v, errs)
				}
			}
		})
	}
}

// Inside a not, dropping part of the subschema makes the not refuse MORE than the schema. A type
// other than the field's own was dropped silently: not: {type: integer, minLength: 2} on a string
// refuses no string (no string is an integer), but the CRD carried not: {minLength: 2} and refused
// every string of length 2 or more. Now the not is dropped, with a warning, and the string admitted.
func TestNotWithAnotherTypeIsDroppedNotNarrowed(t *testing.T) {
	out, warns, err := GenerateWithWarnings(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe",
		SpecSchema: []byte(`{"type":"object","properties":{"s":{"type":"string","not":{"type":"integer","minLength":2}}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(warns, "\n"), "other than the field's string") {
		t.Errorf("warnings %v do not report the type that could not be carried", warns)
	}
	if strings.Contains(string(out), "not:") {
		t.Fatalf("a not was emitted from a subschema whose type could not be carried:\n%s", out)
	}
	crd := generateV1Quiet(t, out)
	if errs := admit(t, crd, probe("s", "abc")); len(errs) != 0 {
		t.Errorf("a string the schema admits was refused: %v", errs)
	}
}

// generateV1Quiet decodes generated CRD bytes without generateV1's no-warnings assertion.
func generateV1Quiet(t *testing.T, out []byte) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	return &crd
}
