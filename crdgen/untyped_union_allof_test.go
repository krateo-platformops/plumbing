package crdgen

import (
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/yaml"
)

// plumbing#186, the two arms #187 left. Both degrade a node crdgen cannot type, and both used to
// degrade to `type: object`, which refuses every string, number, boolean and list the source schema
// accepts:
//   - a oneOf/anyOf of divergent shapes on an untyped node (warned, but the degraded node narrowed);
//   - an allOf with no object member, e.g. [{type: string}, {minLength: 1}] (narrowed, unwarned).
// Both now come out as anyValue(). Every verdict is the apiserver's own validator AND pruner.

// generateDiag generates with diagnostics (a degraded union warns by design) and gates the CRD on
// the apiserver's structural check.
func generateDiag(t *testing.T, spec string) (*apiextensionsv1.CustomResourceDefinition, []Warning) {
	t.Helper()
	out, warns, err := GenerateWithDiagnostics(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(spec)})
	if err != nil {
		t.Fatalf("generation refused: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	if errs := structuralErrs(t, &crd); len(errs) != 0 {
		t.Fatalf("the apiserver would refuse this CRD: %v", errs)
	}
	return &crd, warns
}

// admitAndKeep: admitted by the validator, and unchanged by the pruner.
func admitAndKeep(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, field string, v any) {
	t.Helper()
	obj := probe(field, v)
	if errs := admit(t, crd, obj); len(errs) != 0 {
		t.Errorf("%s = %v refused: %v", field, v, errs)
		return
	}
	scheme := runtime.NewScheme()
	apiextinstall.Install(scheme)
	var internal apiextensions.JSONSchemaProps
	if err := scheme.Convert(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	s, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatalf("not structural: %v", err)
	}
	before, _ := json.Marshal(v)
	pruning.Prune(obj, s, true)
	after, _ := json.Marshal(obj["spec"].(map[string]any)[field])
	if string(before) != string(after) {
		t.Errorf("%s: pruned from %s to %s", field, before, after)
	}
}

const divergentUnion = `{"type":"object","properties":{"u":{"oneOf":[
  {"type":"string"},{"type":"array","items":{"type":"string"}}]}}}`

const allOfNoObject = `{"type":"object","properties":{"s":{"allOf":[{"type":"string"},{"minLength":1}]}}}`

func TestUntypedDivergentUnionAdmitsItsMembers(t *testing.T) {
	crd, warns := generateDiag(t, divergentUnion)
	if len(warns) == 0 {
		t.Error("the divergent union degraded without a warning")
	}
	admitAndKeep(t, crd, "u", "a")
	admitAndKeep(t, crd, "u", []any{"a", "b"})
	// The degraded node still admits what the pre-fix object did, so stored objects stay valid.
	admitAndKeep(t, crd, "u", map[string]any{"k": "v"})
}

func TestAllOfWithoutObjectMemberAdmitsStrings(t *testing.T) {
	crd, _ := generateDiag(t, allOfNoObject)
	admitAndKeep(t, crd, "s", "abc")
	admitAndKeep(t, crd, "s", map[string]any{"k": "v"}) // what the pre-fix object admitted
}

// An allOf that does hold an object keeps it: the object members' fields are still typed.
func TestAllOfWithObjectMemberStaysObject(t *testing.T) {
	crd, _ := generateDiag(t, `{"type":"object","properties":{"o":{"allOf":[
	  {"type":"object","properties":{"n":{"type":"integer"}}},{"required":["n"]}]}}}`)
	if errs := admit(t, crd, probe("o", "a string")); len(errs) == 0 {
		t.Error("an allOf with an object member admitted a string")
	}
	if errs := admit(t, crd, probe("o", map[string]any{"n": "x"})); len(errs) == 0 {
		t.Error("an allOf object member's typed field admitted a string")
	}
	admitAndKeep(t, crd, "o", map[string]any{"n": int64(1)})

	// An object member with no properties and nothing required still makes the merge an object:
	// only the member's type says so, which is what sawObject records.
	bare, _ := generateDiag(t, `{"type":"object","properties":{"b":{"allOf":[{"type":"object"},{"description":"x"}]}}}`)
	if errs := admit(t, bare, probe("b", "a string")); len(errs) == 0 {
		t.Error("an allOf whose only typed member is a bare object admitted a string")
	}
	admitAndKeep(t, bare, "b", map[string]any{"k": "v"})
}

// The roots stay objects even when the root itself is an allOf with no object member or a
// divergent union, which now degrade to anyValue().
func TestRootStaysObjectWhenItDegrades(t *testing.T) {
	for name, spec := range map[string]string{
		"allOf without object member": `{"allOf":[{"if":{"required":["a"]},"then":{"required":["b"]}}]}`,
		"divergent union":             `{"oneOf":[{"type":"string"},{"type":"array"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			crd, _ := generateDiag(t, spec)
			obj := map[string]any{"apiVersion": "composition.krateo.io/v1", "kind": "Probe", "spec": "not-an-object"}
			if errs := admit(t, crd, obj); len(errs) == 0 {
				t.Fatal("a string spec was admitted: the root must stay type object")
			}
		})
	}
}
