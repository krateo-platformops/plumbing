package crdgen

import (
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apimachinery/pkg/runtime"
)

// structuralErrs runs the apiserver's own structural-schema check on a generated CRD: a field the
// apiserver forbids inside a junctor (type, default, x-kubernetes-*) would be reported here, and the
// apiserver would refuse the whole CRD.
func structuralErrs(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) []string {
	t.Helper()
	scheme := runtime.NewScheme()
	apiextinstall.Install(scheme)
	var internal apiextensions.JSONSchemaProps
	if err := scheme.Convert(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	s, err := structuralschema.NewStructural(&internal)
	if err != nil {
		return []string{err.Error()}
	}
	var out []string
	for _, e := range structuralschema.ValidateStructural(nil, s) {
		out = append(out, e.Error())
	}
	return out
}

func probe(field string, v any) map[string]any {
	return map[string]any{"apiVersion": "composition.krateo.io/v1", "kind": "Probe", "spec": map[string]any{field: v}}
}

// plumbing#102. The real case: agentgateway-policies 0.2.20 forbids a sampling value of 2 or more,
// written as a negated pattern, on four string fields. Before the fix the generated CRD carried no
// `not` at all, so "5" was admitted.
const agentgatewaySampling = `{"type":"object","properties":{"tracing":{"type":"object","properties":{
  "randomSampling":{"type":"string","not":{"pattern":"^\\s*(?:[2-9]|[1-9][0-9]+)\\s*$"}},
  "clientSampling":{"type":"string","not":{"pattern":"^\\s*(?:[2-9]|[1-9][0-9]+)\\s*$"}},
  "platform":{"type":"object","properties":{
    "randomSampling":{"type":"string","not":{"pattern":"^\\s*(?:[2-9]|[1-9][0-9]+)\\s*$"}},
    "clientSampling":{"type":"string","not":{"pattern":"^\\s*(?:[2-9]|[1-9][0-9]+)\\s*$"}}}}}}}}`

func TestNotPatternIsNativeAndEnforced(t *testing.T) {
	crd := generateV1(t, agentgatewaySampling) // fails on any warning, so a drop shows here too
	if y := mustYAML(t, crd); strings.Contains(y, "x-kubernetes-validations") {
		t.Error("a CEL rule was emitted; the negated pattern must be a native not")
	}
	if errs := structuralErrs(t, crd); len(errs) != 0 {
		t.Fatalf("the apiserver would refuse this CRD: %v", errs)
	}
	obj := func(v string) map[string]any {
		return probe("tracing", map[string]any{"randomSampling": v, "platform": map[string]any{"clientSampling": v}})
	}
	for _, bad := range []string{"2", " 5 ", "17"} {
		if errs := admit(t, crd, obj(bad)); len(errs) == 0 {
			t.Errorf("sampling %q was ADMITTED: the not-pattern is not enforced", bad)
		}
	}
	// CONTROL: allowed values pass, or the arm above passes against a schema that rejects everything.
	for _, ok := range []string{"0", "1", "0.25"} {
		if errs := admit(t, crd, obj(ok)); len(errs) != 0 {
			t.Errorf("sampling %q was rejected: %v", ok, errs)
		}
	}
}

// Every scalar value validation is carried, and combined with forbidden values.
func TestNotScalarValidationsAreNative(t *testing.T) {
	cases := []struct {
		name     string
		schema   string
		rejected []any
		admitted []any
	}{
		{"minLength", `{"type":"object","properties":{"s":{"type":"string","not":{"minLength":3}}}}`, []any{"abc", "abcd"}, []any{"", "ab"}},
		{"maxLength", `{"type":"object","properties":{"s":{"type":"string","not":{"maxLength":2}}}}`, []any{"", "ab"}, []any{"abc"}},
		{"minimum", `{"type":"object","properties":{"s":{"type":"integer","not":{"minimum":10}}}}`, []any{int64(10), int64(99)}, []any{int64(9)}},
		{"multipleOf", `{"type":"object","properties":{"s":{"type":"integer","not":{"multipleOf":5}}}}`, []any{int64(10)}, []any{int64(7)}},
		{"format", `{"type":"object","properties":{"s":{"type":"string","not":{"format":"ipv4"}}}}`, []any{"10.0.0.1"}, []any{"db.example"}},
		{"enum+pattern", `{"type":"object","properties":{"s":{"type":"string","not":{"enum":["root","admin"],"pattern":"^r"}}}}`, []any{"root"}, []any{"admin", "rx", "bob"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			crd := generateV1(t, c.schema)
			if errs := structuralErrs(t, crd); len(errs) != 0 {
				t.Fatalf("the apiserver would refuse this CRD: %v", errs)
			}
			for _, v := range c.rejected {
				if errs := admit(t, crd, probe("s", v)); len(errs) == 0 {
					t.Errorf("%v was admitted", v)
				}
			}
			for _, v := range c.admitted {
				if errs := admit(t, crd, probe("s", v)); len(errs) != 0 {
					t.Errorf("%v was rejected: %v", v, errs)
				}
			}
		})
	}
}

// What a structural not cannot hold is still dropped with its warning, never emitted as `not: {}`
// (which matches everything and so would reject every value).
func TestNotStillDroppedWhereItMustBe(t *testing.T) {
	for _, c := range []struct{ name, schema, warn string }{
		{"only exclusiveMinimum (emitted nowhere)", `{"type":"object","properties":{"n":{"type":"number","not":{"exclusiveMinimum":3}}}}`, "not (non-trivial) unsupported"},
		{"structured subschema", `{"type":"object","properties":{"s":{"type":"string","not":{"anyOf":[{"pattern":"a"},{"pattern":"b"}]}}}}`, "not (non-trivial subschema) unsupported"},
		{"not on an object", `{"type":"object","properties":{"o":{"type":"object","not":{"required":["x"]}}}}`, "not on non-scalar node unsupported"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, warns, err := GenerateWithWarnings(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(c.schema)})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(warns, "\n"), c.warn) {
				t.Errorf("warnings %v lack %q", warns, c.warn)
			}
			if strings.Contains(string(out), "not:") {
				t.Errorf("a not was emitted where it must be dropped:\n%s", out)
			}
		})
	}
}
