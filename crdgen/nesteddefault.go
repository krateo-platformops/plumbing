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
// the object came into existence. Without this check, materializing is pure noise.
func hasDefaultedDescendant(schema *apiextensionsv1.JSONSchemaProps) bool {
	for name := range schema.Properties {
		prop := schema.Properties[name]
		if prop.Default != nil {
			return true
		}
		if prop.Type == "object" && hasDefaultedDescendant(&prop) {
			return true
		}
	}
	return false
}

// safeToMaterialize reports whether `default: {}` on this object yields a value that still passes
// the object's own validation.
//
// Every required property must end up present: either it carries a default of its own, or it is an
// object we will also materialize (the depth-first walk has already stamped it). A required
// property that would remain absent makes the synthesized `{}` invalid, so the parent must be left
// optional-and-absent instead — which is what the author wrote, and which is valid.
func safeToMaterialize(schema *apiextensionsv1.JSONSchemaProps) bool {
	for _, name := range schema.Required {
		prop, ok := schema.Properties[name]
		if !ok {
			// Required but undeclared: we cannot reason about it, so do not risk it.
			return false
		}
		if prop.Default == nil {
			return false
		}
	}
	return true
}
