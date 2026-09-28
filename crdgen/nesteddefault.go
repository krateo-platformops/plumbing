package crdgen

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// materializeDefaultedParents makes nested defaults reachable when their parent object is omitted.
//
// Kubernetes applies a `default` only to a field whose PARENT exists in the submitted object. So a
// values schema shaped like this:
//
//	spec:
//	  image:            # optional object
//	    tag: "latest"   # has a default
//
// produces a CRD where creating a composition WITHOUT `spec.image` leaves `spec.image.tag` absent
// rather than defaulted. The author has to spell out `image: {}` to get a default they already
// declared, and nothing explains why. The fix is to give the optional parent `default: {}` so the
// apiserver materializes it and then applies the nested defaults — no mutating webhook
// (krateo-platformops/core-provider#138, originally #235).
//
// Two conditions, both necessary:
//
//   - The subtree must actually contain a default. Stamping `default: {}` on every optional object
//     would materialize empty objects nobody asked for, turning an absent field into a present-but-
//     empty one and changing what `has(spec.image)` means for anything reading the object.
//
//   - Materializing must be SAFE. If the object has a required property that would still be missing
//     after materialization, `default: {}` creates an object that fails its own validation — so the
//     apiserver would reject a composition that was previously valid by omission. Those parents are
//     left alone: a nested default is a convenience, and it must never cost validity.
func materializeDefaultedParents(schema *apiextensionsv1.JSONSchemaProps) {
	if schema == nil {
		return
	}

	// Depth-first: an inner parent must have its own default decided before an outer one asks
	// whether materializing it would satisfy an outer required property.
	for name := range schema.Properties {
		prop := schema.Properties[name]
		materializeDefaultedParents(&prop)
		schema.Properties[name] = prop
	}
	if schema.Items != nil && schema.Items.Schema != nil {
		materializeDefaultedParents(schema.Items.Schema)
	}

	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}

	for name := range schema.Properties {
		prop := schema.Properties[name]
		if required[name] {
			// A required parent is always present, so its children's defaults already apply.
			continue
		}
		if prop.Type != "object" || prop.Default != nil {
			continue
		}
		if !hasDefaultedDescendant(&prop) || !safeToMaterialize(&prop) {
			continue
		}
		prop.Default = &apiextensionsv1.JSON{Raw: []byte("{}")}
		schema.Properties[name] = prop
	}
}

// hasDefaultedDescendant reports whether anything inside this object would actually be defaulted if
// the object came into existence — REACHABLY so. Without this check, materializing is pure noise.
//
// Reachability is the subtle half. A default two levels down only applies if the intermediate object
// also comes into existence, which happens when it is required by its parent or carries a default of
// its own. Descending blindly makes a parent look worth materializing because of a default that can
// never be reached, which is how builder-publish's `repository` got `default: {}` for the sake of a
// `name` default sealed behind a configurationRef we correctly refused to materialize (#142).
//
// The depth-first walk in materializeDefaultedParents is what makes this cheap: by the time a parent
// is evaluated, every child object has already had its own default decided, so `prop.Default != nil`
// is an accurate answer to "will this exist".
func hasDefaultedDescendant(schema *apiextensionsv1.JSONSchemaProps) bool {
	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}

	for name := range schema.Properties {
		prop := schema.Properties[name]
		if prop.Default != nil {
			return true
		}
		if prop.Type != "object" {
			continue
		}
		// Only descend where the child will actually exist once this object does.
		if !required[name] {
			continue
		}
		if hasDefaultedDescendant(&prop) {
			return true
		}
	}
	return false
}

// safeToMaterialize reports whether `default: {}` on this object is accepted by the apiserver.
//
// An object with ANY required property cannot be defaulted to `{}`. The apiserver validates a
// default value LITERALLY against its own schema and does not apply nested defaults to it first, so
// `{}` fails `required` even when every required property carries a default of its own.
//
// That distinction is the whole of krateo-platformops/core-provider#142. The first version of this
// guard reasoned that a required property with a default would be filled in, so materializing was
// safe — and shipped. builder-publish has, under two separate parents:
//
//	configurationRef: {type: object, required: ["name"], properties: {name, namespace}}
//
// which produced `default: {}` on configurationRef and a CRD the apiserver rejected outright:
//
//	spec...properties[repository].properties[configurationRef].default.name: Required value
//
// The generated CRD failed validation, so the CompositionDefinition could not sync at all — one bad
// parent takes down the whole definition, not just that field. Defaulting is a convenience; it must
// never cost the CRD its validity.
//
// Not "fixed" by synthesizing `{"name": <default>}` instead. That invents a value the author never
// wrote into a field they marked required, which is precisely the field where guessing is least
// welcome.
func safeToMaterialize(schema *apiextensionsv1.JSONSchemaProps) bool {
	return len(schema.Required) == 0
}
