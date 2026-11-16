package crdgen

import (
	"strings"
	"testing"
)

// An anyOf/oneOf of exactly {type: integer} and {type: string} is int-or-string. Kubernetes writes
// its quantity fields that way (resources.limits/requests), usually with x-kubernetes-int-or-string
// beside it. crdgen took it for a union of divergent shapes and widened it to preserve-unknown:
// with the extension the apiserver refuses the WHOLE CRD (preserve-unknown must be false beside
// int-or-string; 63 such errors in installer's chart schema), and without it the field silently
// accepted any value. Judged by the apiserver's own validator; generateV1 fails on any warning.
const quantityPattern = `^(\\+|-)?(([0-9]+(\\.[0-9]*)?)|(\\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\\+|-)?(([0-9]+(\\.[0-9]*)?)|(\\.[0-9]+))))?$`

func TestIntOrStringJunctionIsIntOrString(t *testing.T) {
	for _, tc := range []struct {
		name, schema      string
		admitted, refused []any
	}{
		{"a Kubernetes quantity map, as installer's chart writes resources.limits",
			`{"type":"object","properties":{"f":{"type":"object","additionalProperties":{"anyOf":[{"type":"integer"},{"type":"string"}],"pattern":"` + quantityPattern + `","x-kubernetes-int-or-string":true}}}}`,
			[]any{map[string]any{"cpu": "500m", "memory": "2Gi"}, map[string]any{"cpu": 2}},
			[]any{map[string]any{"cpu": "lots"}, map[string]any{"cpu": true}, map[string]any{"cpu": map[string]any{}}}},
		{"anyOf without the extension",
			`{"type":"object","properties":{"f":{"anyOf":[{"type":"integer"},{"type":"string"}]}}}`,
			[]any{1, "x"}, []any{true, map[string]any{}, []any{"x"}}},
		{"oneOf, members in the other order",
			`{"type":"object","properties":{"f":{"oneOf":[{"type":"string"},{"type":"integer"}],"x-kubernetes-int-or-string":true}}}`,
			[]any{1, "x"}, []any{true, 1.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crd := generateV1(t, tc.schema)
			if errs := structuralErrs(t, crd); len(errs) != 0 {
				t.Fatalf("the apiserver would refuse this CRD: %v", errs)
			}
			for _, v := range tc.admitted {
				if errs := admit(t, crd, probe("f", asDecoded(t, v))); len(errs) != 0 {
					t.Errorf("%v was refused: %v", v, errs)
				}
			}
			for _, v := range tc.refused {
				if errs := admit(t, crd, probe("f", asDecoded(t, v))); len(errs) == 0 {
					t.Errorf("%v was ADMITTED: the field is not int-or-string", v)
				}
			}
		})
	}
}

// CONTROL: number|string is not int-or-string (the narrowing isIntOrStringUnion also keeps), so it
// still goes through attachUnion's divergent-union path and says so.
func TestNumberOrStringJunctionIsNotIntOrString(t *testing.T) {
	out, warns, err := GenerateWithWarnings(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe",
		SpecSchema: []byte(`{"type":"object","properties":{"f":{"anyOf":[{"type":"number"},{"type":"string"}]}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "x-kubernetes-int-or-string") {
		t.Error("number|string was emitted as int-or-string")
	}
	if !strings.Contains(strings.Join(warns, "\n"), "divergent shapes") {
		t.Errorf("warnings %v do not report the union", warns)
	}
}

// Defence in depth: on a node that is already int-or-string, a union crdgen cannot express is never
// widened to preserve-unknown (which the apiserver refuses beside int-or-string). It is dropped and
// reported, and the CRD stays valid.
func TestDivergentUnionOnAnIntOrStringNodeKeepsTheCRDValid(t *testing.T) {
	out, warns, err := GenerateWithDiagnostics(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe",
		SpecSchema: []byte(`{"type":"object","properties":{"f":{"anyOf":[{"type":"integer","minimum":1},{"type":"string","pattern":"^a"}],"x-kubernetes-int-or-string":true}}}`)})
	if err != nil {
		t.Fatalf("generation refused (the CRD is invalid): %v", err)
	}
	found := false
	for _, w := range warns {
		if w.Kind == KindConstraintDropped && strings.Contains(w.Message, "int-or-string node") {
			found = true
		}
	}
	if !found {
		t.Errorf("no ConstraintDropped warning for the dropped union: %v", warns)
	}
	crd := generateV1Quiet(t, out)
	if errs := structuralErrs(t, crd); len(errs) != 0 {
		t.Fatalf("the apiserver would refuse this CRD: %v", errs)
	}
	for _, v := range []any{5, "abc"} {
		if errs := admit(t, crd, probe("f", asDecoded(t, v))); len(errs) != 0 {
			t.Errorf("%v was refused: %v", v, errs)
		}
	}
}
