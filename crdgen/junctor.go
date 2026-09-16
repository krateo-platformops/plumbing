package crdgen

import (
	"reflect"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/krateo-platformops/plumbing/crdgen/schemas"
)

// Kubernetes structural schemas split a schema into STRUCTURE (type, properties, items,
// additionalProperties, the x-kubernetes-* annotations) and VALUE VALIDATIONS (required, enum,
// minimum, pattern, ...). Inside a logical junctor — anyOf / oneOf / allOf / not — only value
// validations are allowed; every structural field must be absent. The apiserver enforces this and
// rejects the WHOLE CRD otherwise, e.g.
//
//	spec.validation.openAPIV3Schema.properties[spec].anyOf[0].type: Forbidden: must be empty to be structural
//	spec.validation.openAPIV3Schema.properties[spec].anyOf[0].x-kubernetes-preserve-unknown-fields: Forbidden: must be false to be structural
//
// which froze builder-publish at v1-8-23: the CompositionDefinition declared chart 1.8.24, the
// generated CRD was refused, and the old version kept serving — silently, through every later bump.
//
// crdgen hit this because union members are transpiled by the normal path: a constraint-only member
// (say `{"required": ["a"]}`) has no usable type, so transpile falls through to openObject() and
// stamps `type: object` + `x-kubernetes-preserve-unknown-fields: true` onto it — precisely the two
// forbidden fields, once per member.
//
// sanitizeJunctorMember strips every field the apiserver forbids inside a junctor, recursively
// (members may nest properties, items and further junctors, and the rule applies at every depth).
// The list mirrors apiextensions' validateNestedValueValidation.
func sanitizeJunctorMember(s *apiextensionsv1.JSONSchemaProps) {
	if s == nil {
		return
	}

	// Structure — forbidden inside a junctor. type is hoisted to the parent by the caller, which is
	// where it belongs and where it still constrains the value.
	s.Type = ""
	s.AdditionalProperties = nil
	s.Default = nil
	s.Nullable = false

	// x-kubernetes-* extensions — all forbidden inside a junctor.
	s.XPreserveUnknownFields = nil
	s.XEmbeddedResource = false
	s.XIntOrString = false
	s.XListMapKeys = nil
	s.XListType = nil
	s.XMapType = nil
	s.XValidations = nil

	for k, v := range s.Properties {
		sanitizeJunctorMember(&v)
		s.Properties[k] = v
	}
	if s.Items != nil {
		sanitizeJunctorMember(s.Items.Schema)
		for i := range s.Items.JSONSchemas {
			sanitizeJunctorMember(&s.Items.JSONSchemas[i])
		}
	}
	for i := range s.AllOf {
		sanitizeJunctorMember(&s.AllOf[i])
	}
	for i := range s.AnyOf {
		sanitizeJunctorMember(&s.AnyOf[i])
	}
	for i := range s.OneOf {
		sanitizeJunctorMember(&s.OneOf[i])
	}
	sanitizeJunctorMember(s.Not)
}

// junctorMemberSchema builds a junctor member from VALUE VALIDATIONS ONLY, which is all a structural
// schema permits there.
//
// It deliberately does not reuse the main transpile path. That path is structure-first: a member with
// no `type` — which every constraint-only member is, by definition — falls through to openObject()
// and comes back as `{type: object, x-kubernetes-preserve-unknown-fields: true}` with its actual
// constraint DISCARDED. Stripping the forbidden fields afterwards would leave `{}`, so the union
// would have to be dropped and `anyOf: [{required:[a]},{required:[b]}]` — a perfectly legal
// structural schema — would be silently lost. Building the member directly keeps the constraint and
// keeps the CRD valid.
func junctorMemberSchema(node *schemas.Type) *apiextensionsv1.JSONSchemaProps {
	if node == nil {
		return nil
	}
	out := &apiextensionsv1.JSONSchemaProps{}
	if len(node.Required) > 0 {
		out.Required = append([]string(nil), node.Required...)
	}
	copyScalarValidation(out, node)
	if node.Description != "" {
		out.Description = node.Description
	}
	return out
}

// isVacuousJunctorMember reports whether a sanitized member still constrains anything. Stripping the
// structural fields can leave a member empty (the openObject() degradation above sanitizes to `{}`),
// and an empty member matches EVERYTHING — so an anyOf containing one is satisfied by any value at
// all. Emitting that would be a validation no-op dressed up as a constraint; the caller drops the
// junctor instead and warns, which is honest about what was lost.
//
// Description and Title are ignored: they document, they do not validate.
func isVacuousJunctorMember(s apiextensionsv1.JSONSchemaProps) bool {
	s.Description = ""
	s.Title = ""
	s.Example = nil
	s.ExternalDocs = nil

	return reflect.DeepEqual(s, apiextensionsv1.JSONSchemaProps{})
}
