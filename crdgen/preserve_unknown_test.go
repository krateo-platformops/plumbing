package crdgen

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func preserved(s apiextensionsv1.JSONSchemaProps) bool {
	return s.XPreserveUnknownFields != nil && *s.XPreserveUnknownFields
}

func markerProp(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, name string) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	p, ok := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties[name]
	if !ok {
		t.Fatalf("spec.%s is missing from the generated CRD", name)
	}
	return p
}

// THE REGRESSION (#88). An object declaring properties took the fillObject path, the marker was never
// read, and the apiserver pruned every undeclared field on write. Silent data loss.
func TestPreserveUnknown_KeptAlongsideProperties(t *testing.T) {
	crd := genCRD(t, `{"type":"object","properties":{
		"mixed":{"type":"object","x-kubernetes-preserve-unknown-fields":true,
		         "properties":{"known":{"type":"string"}}}
	}}`)

	mixed := markerProp(t, crd, "mixed")
	if !preserved(mixed) {
		t.Fatal("the marker was dropped from an object with properties")
	}
	if _, ok := mixed.Properties["known"]; !ok {
		t.Fatal("honouring the marker must not lose the declared properties")
	}
}

// The control: nothing opens an object that did not ask for it.
func TestPreserveUnknown_ClosedObjectStaysClosed(t *testing.T) {
	crd := genCRD(t, `{"type":"object","properties":{
		"closed":{"type":"object","properties":{"known":{"type":"string"}}}
	}}`)

	if preserved(markerProp(t, crd, "closed")) {
		t.Fatal("an object without the marker was opened")
	}
}

// $ref returns before the common keywords, so a sibling marker needs its own handling.
func TestPreserveUnknown_KeptBesideARef(t *testing.T) {
	crd := genCRD(t, `{"type":"object",
		"$defs":{"thing":{"type":"object","properties":{"known":{"type":"string"}}}},
		"properties":{
			"viaRef":{"$ref":"#/$defs/thing","x-kubernetes-preserve-unknown-fields":true},
			"plainRef":{"$ref":"#/$defs/thing"}
		}}`)

	if !preserved(markerProp(t, crd, "viaRef")) {
		t.Fatal("a marker beside $ref was dropped")
	}
	if preserved(markerProp(t, crd, "plainRef")) {
		t.Fatal("the marker leaked onto another use of the same $ref target")
	}
}

func TestPreserveUnknown_KeptThroughAllOf(t *testing.T) {
	crd := genCRD(t, `{"type":"object","properties":{
		"merged":{"type":"object","x-kubernetes-preserve-unknown-fields":true,
		          "properties":{"base":{"type":"string"}},
		          "allOf":[{"properties":{"known":{"type":"string"}}}]}
	}}`)

	if !preserved(markerProp(t, crd, "merged")) {
		t.Fatal("a marker on an allOf base was dropped")
	}
}

// Forbidden inside a junctor: the apiserver refuses the CRD, which is the builder-publish incident.
func TestPreserveUnknown_NotCarriedIntoAJunctor(t *testing.T) {
	crd := genCRD(t, `{"type":"object","properties":{
		"choice":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},
		          "anyOf":[{"x-kubernetes-preserve-unknown-fields":true,"required":["a"]},{"required":["b"]}]}
	}}`)

	// A typed member would drop the union and pass this loop over zero members.
	members := markerProp(t, crd, "choice").AnyOf
	if len(members) != 2 {
		t.Fatalf("want the anyOf kept with 2 members, got %d", len(members))
	}
	for i, m := range members {
		if preserved(m) {
			t.Fatalf("anyOf[%d] carries the marker, which the apiserver forbids", i)
		}
	}
}

// The apiserver takes only true or undefined here, so false must not reach the output. It also must
// not close an object that is open for having no properties: that is what it has always done.
func TestPreserveUnknown_FalseIsNotEmitted(t *testing.T) {
	crd := genCRD(t, `{"type":"object","properties":{
		"withProps":{"type":"object","x-kubernetes-preserve-unknown-fields":false,
		             "properties":{"known":{"type":"string"}}}
	}}`)

	if markerProp(t, crd, "withProps").XPreserveUnknownFields != nil {
		t.Fatal("x-kubernetes-preserve-unknown-fields: false reached the output")
	}
}

// The marker has only ever been ignored on a scalar. A chart that carries it there generates today,
// so honouring it must not change that output.
func TestPreserveUnknown_IgnoredOnAScalar(t *testing.T) {
	crd := genCRD(t, `{"type":"object","properties":{
		"name":{"type":"string","x-kubernetes-preserve-unknown-fields":true}
	}}`)

	if preserved(markerProp(t, crd, "name")) {
		t.Fatal("the marker was applied to a string")
	}
}
