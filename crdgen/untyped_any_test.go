package crdgen

import "testing"

// plumbing#186. JSON Schema's `{}` (a node with no usable type) means "any value". crdgen used to
// emit `type: object` + x-kubernetes-preserve-unknown-fields for it, so the apiserver refused every
// string, number, boolean and list written there ("must be of type object"), with no warning. The
// real case: trust-manager v0.20.0's app.targetNamespaces, `{"type":"array","items":{}}` behind
// $ref/$defs, written as a list of namespace names (installer PR #948).
//
// Each value is judged by the apiserver's OWN validator (admit), and the CRD by its OWN structural
// check (structuralErrs): the shape of the emitted schema is not the claim, admission is.
func TestUntypedNodeAdmitsAnyValue(t *testing.T) {
	const trustManagerShape = `{"type":"object","properties":{"app":{"$ref":"#/$defs/helm-values.app"}},
	  "$defs":{"helm-values.app":{"type":"object","properties":{"targetNamespaces":{"$ref":"#/$defs/helm-values.app.targetNamespaces"}}},
	           "helm-values.app.targetNamespaces":{"type":"array","items":{}}}}`
	const bareAny = `{"type":"object","properties":{"anything":{}}}`
	const arrayNoItems = `{"type":"object","properties":{"list":{"type":"array"}}}`

	values := map[string]any{
		"string":  "cert-manager",
		"number":  3.5,
		"integer": int64(3),
		"boolean": true,
		"list":    []any{"a", int64(1)},
		"object":  map[string]any{"x": "y"},
	}

	t.Run("array items {} behind $ref/$defs (trust-manager)", func(t *testing.T) {
		crd := generateV1(t, trustManagerShape)
		if errs := structuralErrs(t, crd); len(errs) != 0 {
			t.Fatalf("the apiserver would refuse this CRD: %v", errs)
		}
		for name, v := range values {
			obj := probe("app", map[string]any{"targetNamespaces": []any{v}})
			if errs := admit(t, crd, obj); len(errs) != 0 {
				t.Errorf("an item of kind %s was refused: %v", name, errs)
			}
		}
	})
	t.Run("bare {} field", func(t *testing.T) {
		crd := generateV1(t, bareAny)
		if errs := structuralErrs(t, crd); len(errs) != 0 {
			t.Fatalf("the apiserver would refuse this CRD: %v", errs)
		}
		for name, v := range values {
			if errs := admit(t, crd, probe("anything", v)); len(errs) != 0 {
				t.Errorf("a value of kind %s was refused: %v", name, errs)
			}
		}
	})
	t.Run("array with no items keyword", func(t *testing.T) {
		crd := generateV1(t, arrayNoItems)
		if errs := structuralErrs(t, crd); len(errs) != 0 {
			t.Fatalf("the apiserver would refuse this CRD: %v", errs)
		}
		for name, v := range values {
			if errs := admit(t, crd, probe("list", []any{v})); len(errs) != 0 {
				t.Errorf("an item of kind %s was refused: %v", name, errs)
			}
		}
	})
	// Control: the instrument refuses a wrong kind where a type IS declared, so a pass above is not
	// a validator that admits everything.
	t.Run("control: typed items still refuse a wrong kind", func(t *testing.T) {
		crd := generateV1(t, `{"type":"object","properties":{"list":{"type":"array","items":{"type":"string"}}}}`)
		if errs := admit(t, crd, probe("list", []any{int64(1)})); len(errs) == 0 {
			t.Fatal("an integer item was admitted into a string list: the validator is not judging")
		}
		if errs := admit(t, crd, probe("list", []any{"ok"})); len(errs) != 0 {
			t.Fatalf("a string item was refused by a string list: %v", errs)
		}
	})
	// The spec root itself stays an object: a non-object spec must still be refused.
	t.Run("control: an untyped spec root is still an object", func(t *testing.T) {
		crd := generateV1(t, `{}`)
		obj := map[string]any{"apiVersion": "composition.krateo.io/v1", "kind": "Probe", "spec": "not-an-object"}
		if errs := admit(t, crd, obj); len(errs) == 0 {
			t.Fatal("a string spec was admitted: the root must stay type object")
		}
	})
}
