package crdgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// genCRD transpiles a spec-schema and returns the generated CRD. A non-nil Generate error means the
// apiserver validation gate rejected it, so these tests double as proof the CRD is structural.
func genCRD(t *testing.T, specSchema string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	out, err := Generate(Options{
		Group:        "composition.krateo.io",
		Version:      "v1-8-24",
		Kind:         "BuilderPublish",
		SpecSchema:   []byte(specSchema),
		StatusSchema: []byte(defaultStatusSchema),
	})
	if err != nil {
		t.Fatalf("Generate failed (apiserver gate rejected the CRD): %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatalf("unmarshal generated CRD: %v", err)
	}
	return &crd
}

// The builder-publish regression. A spec-level anyOf expressing "either a template source or none"
// produced members that the transpiler stamped `type: object` + x-kubernetes-preserve-unknown-fields
// onto, both FORBIDDEN inside a junctor. The apiserver refused the CRD with four structural errors,
// so composition.krateo.io/v1-8-24 was never created and builder-publish stayed frozen serving
// v1-8-23 while its CompositionDefinition declared chart 1.8.24.
func TestAnyOf_SpecLevelIsStructural(t *testing.T) {
	crd := genCRD(t, `{
		"type":"object",
		"properties":{
			"sourceRepository":{"type":"object","properties":{"url":{"type":"string"}}},
			"name":{"type":"string"}
		},
		"anyOf":[
			{"required":["sourceRepository"]},
			{"required":["name"]}
		]
	}`)

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if len(spec.AnyOf) != 2 {
		t.Fatalf("expected the anyOf to survive with 2 members, got %d", len(spec.AnyOf))
	}
	for i, m := range spec.AnyOf {
		if m.Type != "" {
			t.Errorf("anyOf[%d].type = %q, must be empty to be structural", i, m.Type)
		}
		if m.XPreserveUnknownFields != nil && *m.XPreserveUnknownFields {
			t.Errorf("anyOf[%d].x-kubernetes-preserve-unknown-fields must not be set inside a junctor", i)
		}
	}
	// The constraint itself must be preserved, not merely made legal by deleting it.
	got := []string{strings.Join(spec.AnyOf[0].Required, ","), strings.Join(spec.AnyOf[1].Required, ",")}
	if got[0] != "sourceRepository" || got[1] != "name" {
		t.Errorf("anyOf required constraints lost: %v", got)
	}
	// type belongs on the parent, where it is legal and still enforced.
	if spec.Type != "object" {
		t.Errorf("parent spec.type = %q, want object", spec.Type)
	}
}

// oneOf takes the same path and must be sanitized identically.
func TestOneOf_SpecLevelIsStructural(t *testing.T) {
	crd := genCRD(t, `{
		"type":"object",
		"properties":{"a":{"type":"string"},"b":{"type":"string"}},
		"oneOf":[{"required":["a"]},{"required":["b"]}]
	}`)
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if len(spec.OneOf) != 2 {
		t.Fatalf("expected 2 oneOf members, got %d", len(spec.OneOf))
	}
	for i, m := range spec.OneOf {
		if m.Type != "" || (m.XPreserveUnknownFields != nil && *m.XPreserveUnknownFields) {
			t.Errorf("oneOf[%d] still carries structure: type=%q preserveUnknown=%v", i, m.Type, m.XPreserveUnknownFields)
		}
	}
}

// A junctor whose members carry no expressible constraint would sanitize to {}, which matches
// everything and makes the union a silent no-op. It must be dropped rather than emitted, and the
// CRD must still be valid.
func TestAnyOf_VacuousMembersDropped(t *testing.T) {
	crd := genCRD(t, `{
		"type":"object",
		"properties":{"a":{"type":"string"}},
		"anyOf":[{"description":"one"},{"description":"two"}]
	}`)
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if len(spec.AnyOf) != 0 {
		t.Errorf("a union of unconstraining members must be dropped, got %d members", len(spec.AnyOf))
	}
}

// The real builder-publish values.schema.json (chart 1.8.24) that froze the component on 057. Its
// spec-level anyOf is `[{"required":["files"]},{"required":["filesBundle"]}]` — an either/or over two
// template sources. Before the fix Generate returned the apiserver's four structural errors and
// composition.krateo.io/v1-8-24 was never created.
func TestAnyOf_BuilderPublishFixture(t *testing.T) {
	schema, err := os.ReadFile(filepath.Join("testdata", "builder-publish.values.schema.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out, err := Generate(Options{
		Group:        "composition.krateo.io",
		Version:      "v1-8-24",
		Kind:         "BuilderPublish",
		SpecSchema:   schema,
		StatusSchema: []byte(defaultStatusSchema),
	})
	if err != nil {
		t.Fatalf("Generate failed on the real builder-publish 1.8.24 schema (the reported blocker): %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatalf("unmarshal generated CRD: %v", err)
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	if len(spec.AnyOf) != 2 {
		t.Fatalf("expected the files/filesBundle anyOf to survive, got %d members", len(spec.AnyOf))
	}
	seen := map[string]bool{}
	for i, m := range spec.AnyOf {
		if m.Type != "" || (m.XPreserveUnknownFields != nil && *m.XPreserveUnknownFields) {
			t.Errorf("anyOf[%d] still structural: type=%q preserveUnknown=%v", i, m.Type, m.XPreserveUnknownFields)
		}
		for _, r := range m.Required {
			seen[r] = true
		}
	}
	if !seen["files"] || !seen["filesBundle"] {
		t.Errorf("the either/or constraint was lost; required seen = %v", seen)
	}
}

// sanitizeJunctorMember must recurse: nested properties, items and nested junctors are all subject
// to the same rule, and a single survivor anywhere in the subtree fails the whole CRD.
func TestSanitizeJunctorMember_Recurses(t *testing.T) {
	yes := true
	s := apiextensionsv1.JSONSchemaProps{
		Type:                   "object",
		XPreserveUnknownFields: &yes,
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"nested": {Type: "string", XPreserveUnknownFields: &yes},
		},
		Items: &apiextensionsv1.JSONSchemaPropsOrArray{
			Schema: &apiextensionsv1.JSONSchemaProps{Type: "integer", XEmbeddedResource: true},
		},
		AnyOf: []apiextensionsv1.JSONSchemaProps{{Type: "boolean", XIntOrString: true}},
	}

	sanitizeJunctorMember(&s)

	if s.Type != "" || s.XPreserveUnknownFields != nil {
		t.Errorf("top level not sanitized: type=%q preserve=%v", s.Type, s.XPreserveUnknownFields)
	}
	if n := s.Properties["nested"]; n.Type != "" || n.XPreserveUnknownFields != nil {
		t.Errorf("nested property not sanitized: %+v", n)
	}
	if s.Items.Schema.Type != "" || s.Items.Schema.XEmbeddedResource {
		t.Errorf("items schema not sanitized: %+v", s.Items.Schema)
	}
	if s.AnyOf[0].Type != "" || s.AnyOf[0].XIntOrString {
		t.Errorf("nested junctor member not sanitized: %+v", s.AnyOf[0])
	}
}
