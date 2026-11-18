package valuesschema

import "strconv"

// Opening a subchart schema's objects where the author left `additionalProperties` unstated
// (core-provider#568 child 2's round-trip gate; Diego, 2026-11-18: "open where unstated").
//
// JSON Schema, and so helm, treats an object whose `additionalProperties` is absent as OPEN: a
// values key the schema does not declare passes validation and reaches the templates. crdgen
// translates the same object CLOSED (properties without x-kubernetes-preserve-unknown-fields), so
// the apiserver silently prunes that key from a composition. The round-trip gate measured it on the
// fixtures: svc.undeclared, svc.tls.undeclared and every helm-propagated <subchart>.global came back
// missing. Inferred maps are already typed-open (D3), so an embedded subchart schema is the only
// place an inferred CRD could prune a value helm accepts.
//
// Rules:
//   - An object node with `properties` and no `additionalProperties` gets `additionalProperties:
//     true`, which crdgen carries as x-kubernetes-preserve-unknown-fields (transpile.go). A node that
//     already carries x-kubernetes-preserve-unknown-fields is left alone.
//   - An author's explicit `additionalProperties` (false, or a schema) is never touched: a closed
//     object stays closed, as helm also refuses its undeclared keys.
//   - Only positions that describe the VALUE are walked: properties, patternProperties, items,
//     additionalProperties (schema form), $defs/definitions, allOf/anyOf/oneOf. `not`, if/then/else
//     and dependentSchemas are not: opening an object inside `not` would narrow what the schema
//     accepts, the opposite of the intent.
//   - Each opened node is reported as KindAuthoredMapOpened, Path the dependency's values path and
//     Detail the node's JSON pointer in the output document, in document order.
//
// This only loosens what an embedded subtree accepts, which RulesVersion allows within a version.

// openUnstated returns tree with every unstated object opened, reporting each at path.
func (in *inferrer) openUnstated(tree any, path, ptr string) any {
	o, ok := tree.(orderedObject)
	if !ok {
		return tree
	}
	if _, hasProps := o.get("properties"); hasProps {
		_, hasAddl := o.get("additionalProperties")
		_, hasPreserve := o.get("x-kubernetes-preserve-unknown-fields")
		if !hasAddl && !hasPreserve {
			o = append(o, kv{"additionalProperties", true})
			in.find(path, KindAuthoredMapOpened, ptr)
		}
	}
	for i, e := range o {
		switch e.k {
		case "properties", "patternProperties", "$defs", "definitions":
			if m, ok := e.v.(orderedObject); ok {
				for j, c := range m {
					m[j].v = in.openUnstated(c.v, path, ptr+"/"+e.k+"/"+pointerEscape(c.k))
				}
			}
		case "items":
			switch t := e.v.(type) {
			case orderedObject:
				o[i].v = in.openUnstated(t, path, ptr+"/items")
			case []any:
				for j := range t {
					t[j] = in.openUnstated(t[j], path, ptr+"/items/"+strconv.Itoa(j))
				}
			}
		case "additionalProperties":
			if _, isSchema := e.v.(orderedObject); isSchema {
				o[i].v = in.openUnstated(e.v, path, ptr+"/additionalProperties")
			}
		case "allOf", "anyOf", "oneOf":
			if list, ok := e.v.([]any); ok {
				for j := range list {
					list[j] = in.openUnstated(list[j], path, ptr+"/"+e.k+"/"+strconv.Itoa(j))
				}
			}
		}
	}
	return o
}
