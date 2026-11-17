package crdgen

import (
	"strings"
	"testing"
)

// plumbing#48: a `not` over an OBJECT's own value validations is carried natively. core-provider's
// chart forbids two keys in its cdc env map; that was the last `not` crdgen dropped across 1,187
// local values schemas.
const envForbidsKeys = `{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"},
  "not":{"anyOf":[{"required":["GOMEMLIMIT"]},{"required":["COMPOSITION_CONTROLLER_HELM_TIMEOUT"]}]}}}}`

func envCR(env map[string]any) map[string]any {
	return map[string]any{"apiVersion": "composition.krateo.io/v1", "kind": "Probe", "spec": map[string]any{"env": env}}
}

func TestNotOverRequiredOnAnObjectIsNativeAndEnforced(t *testing.T) {
	crd := generateV1(t, envForbidsKeys) // fails on any warning: nothing may be dropped
	if strings.Contains(mustYAML(t, crd), "x-kubernetes-validations") {
		t.Error("carried as CEL; a structural schema takes required inside a not natively")
	}
	for _, forbidden := range []string{"GOMEMLIMIT", "COMPOSITION_CONTROLLER_HELM_TIMEOUT"} {
		if errs := admit(t, crd, envCR(map[string]any{"OTHER": "1", forbidden: "x"})); len(errs) == 0 {
			t.Errorf("env with %s was ADMITTED: the forbidden key is not enforced", forbidden)
		}
	}
	if errs := admit(t, crd, envCR(map[string]any{"OTHER": "1"})); len(errs) != 0 {
		t.Errorf("an allowed env was rejected: %v", errs)
	}
}

// The property-count bounds are the object's own and are carried; their loss inside a not would make
// it refuse MORE. not: {maxProperties: 1} means "at least two properties".
func TestNotOverPropertyCountOnAnObject(t *testing.T) {
	crd := generateV1(t, `{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"type":"string"},"not":{"maxProperties":1}}}}`)
	m := func(n int) map[string]any {
		out := map[string]any{}
		for i := 0; i < n; i++ {
			out[string(rune('a'+i))] = "v"
		}
		return map[string]any{"apiVersion": "composition.krateo.io/v1", "kind": "Probe", "spec": map[string]any{"m": out}}
	}
	if errs := admit(t, crd, m(1)); len(errs) == 0 {
		t.Error("one property was admitted; not {maxProperties: 1} requires at least two")
	}
	if errs := admit(t, crd, m(2)); len(errs) != 0 {
		t.Errorf("two properties were refused: %v", errs)
	}
}

// What an object's not still cannot carry drops the WHOLE not, reported, never a part of it.
func TestObjectNotWithAnUncarriableKeywordIsDroppedWhole(t *testing.T) {
	for _, tc := range []struct{ name, not string }{
		{"patternProperties", `{"patternProperties":{"^x-":{"type":"string"}}}`},
		{"dependentRequired", `{"dependentRequired":{"a":["b"]}}`},
		{"properties", `{"properties":{"a":{"const":"x"}}}`},
		// Beside a carriable keyword: carrying only `required` would make the not refuse MORE.
		{"required beside patternProperties", `{"required":["a"],"patternProperties":{"^x-":{"type":"string"}}}`},
		{"required beside dependentRequired", `{"required":["a"],"dependentRequired":{"a":["b"]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ws, err := GenerateWithDiagnostics(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe",
				SpecSchema: []byte(`{"type":"object","properties":{"o":{"type":"object","additionalProperties":{"type":"string"},"not":` + tc.not + `}}}`)})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, w := range ws {
				found = found || (w.Kind == KindConstraintDropped && w.Path == "spec.o" && strings.Contains(w.Message, "not"))
			}
			if !found {
				t.Errorf("want the not on spec.o dropped and reported, got %v", ws)
			}
		})
	}
}
