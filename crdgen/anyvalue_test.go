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

// plumbing#186. JSON Schema's `{}` accepts every value. crdgen used to emit it as
// `{type: object, x-kubernetes-preserve-unknown-fields: true}`, so the apiserver refused every
// string, number, boolean and list with "must be of type object" — and nothing warned. The live
// case is trust-manager 0.20.0's `app.targetNamespaces` ({"type":"array","items":{}} behind
// $ref/$defs): installer derives it as ["krateo-system"], and the composition write was refused.
//
// Every assertion here goes through the apiserver's OWN validator and pruner, never through the
// generated schema's shape: the shape is what was wrong, and a test reading it would have agreed.

// anyValues is one value of every JSON type. Each must be admitted, and survive pruning, wherever
// the source schema says `{}`.
var anyValues = map[string]any{
	"string":  "cert-manager",
	"integer": int64(3),
	"number":  1.5,
	"boolean": true,
	"list":    []any{"a", int64(1)},
	"object":  map[string]any{"x": "y"},
}

// admitAndKeep validates obj with the apiserver's schema validator, then prunes it with the
// apiserver's pruner, and reports whether spec[field] came through unchanged.
func admitAndKeep(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, field string, v any) {
	t.Helper()
	obj := probe(field, v)
	if errs := admit(t, crd, obj); len(errs) != 0 {
		t.Errorf("%s %v refused: %v", field, v, errs)
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

func generateWarned(t *testing.T, spec string) (*apiextensionsv1.CustomResourceDefinition, []Warning) {
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

// The reported case, in trust-manager's own shape: $ref -> $defs -> array with `items: {}`.
const trustManagerTargetNamespaces = `{"type":"object","properties":{
  "app":{"type":"object","properties":{"targetNamespaces":{"$ref":"#/$defs/helm-values.app.targetNamespaces"}}}},
  "$defs":{"helm-values.app.targetNamespaces":{"description":"namespaces trust-manager writes to","type":"array","items":{}}}}`

func TestAnyValue_TrustManagerTargetNamespaces(t *testing.T) {
	crd := generateV1(t, trustManagerTargetNamespaces) // exact mapping: no warning expected
	for _, ns := range [][]any{{"krateo-system"}, {"krateo-system", "cert-manager"}} {
		admitAndKeep(t, crd, "app", map[string]any{"targetNamespaces": ns})
	}
	for name, v := range anyValues {
		t.Run(name, func(t *testing.T) {
			admitAndKeep(t, crd, "app", map[string]any{"targetNamespaces": []any{v}})
		})
	}
}

func TestAnyValue_UntypedProperty(t *testing.T) {
	crd := generateV1(t, `{"type":"object","properties":{"anything":{"description":"no type"}}}`)
	for name, v := range anyValues {
		t.Run(name, func(t *testing.T) { admitAndKeep(t, crd, "anything", v) })
	}
}

func TestAnyValue_ArrayWithoutItems(t *testing.T) {
	crd := generateV1(t, `{"type":"object","properties":{"list":{"type":"array"}}}`)
	for name, v := range anyValues {
		t.Run(name, func(t *testing.T) { admitAndKeep(t, crd, "list", []any{v}) })
	}
}

// A union whose members cannot be merged structurally degrades with a warning, and the degraded
// node must still accept what the members accept.
func TestAnyValue_DivergentUnion(t *testing.T) {
	crd, warns := generateWarned(t, `{"type":"object","properties":{"u":{"oneOf":[
	  {"type":"string"},{"type":"array","items":{"type":"string"}}]}}}`)
	if len(warns) == 0 {
		t.Error("the divergent union degraded without a warning")
	}
	admitAndKeep(t, crd, "u", "a")
	admitAndKeep(t, crd, "u", []any{"a"})
}

// allOf with no object member has no object to merge into, so it must not become one.
func TestAnyValue_AllOfWithoutObjectMember(t *testing.T) {
	crd, _ := generateWarned(t, `{"type":"object","properties":{"s":{"allOf":[{"type":"string"},{"minLength":1}]}}}`)
	admitAndKeep(t, crd, "s", "abc")
}

// The roots stay objects: a Helm values root is a map, and generation builds on that shape.
func TestAnyValue_UntypedRootStaysObject(t *testing.T) {
	crd := generateV1(t, `{}`)
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if spec.Type != "object" {
		t.Errorf("spec root type = %q, want object", spec.Type)
	}
	admitAndKeep(t, crd, "x", "y")
}

// Controls: the validator is not blind. Typed nodes still refuse values of another type, so the
// admissions above are the fix and not an instrument that admits everything.
func TestAnyValue_TypedNodesStillRefuse(t *testing.T) {
	crd := generateV1(t, `{"type":"object","properties":{
	  "obj":{"type":"object"},
	  "strs":{"type":"array","items":{"type":"string"}}}}`)
	if errs := admit(t, crd, probe("obj", "a string")); len(errs) == 0 {
		t.Error("an object field admitted a string: the validator is not checking types")
	}
	if errs := admit(t, crd, probe("strs", []any{int64(1)})); len(errs) == 0 {
		t.Error("a string list admitted an integer: the validator is not checking types")
	}
}
