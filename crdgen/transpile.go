package crdgen

// Direct JSON-Schema -> structural-OpenAPI-v3 transpiler.
//
// This is the faithful replacement for the JSON-Schema -> Go-structs -> controller-gen -> CRD
// round-trip (see crdgen/docs/ref-resolution-redesign.md). It walks the parsed schema, inlines
// $refs by JSON pointer (never a folded Go identifier, so no name collisions), breaks cycles with
// x-kubernetes-preserve-unknown-fields, preserves map value schemas, keeps only constraint-only
// anyOf/oneOf (degrading true unions explicitly and locally), and copies validation keywords
// straight into a structural OpenAPI v3 schema. The result is gated on the API server's own
// validator so the generator can never emit a CRD the apiserver would reject.
//
// Selected by CRDGEN_TRANSPILER=direct (wired in crdgen.go Generate).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gobuffalo/flect"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/krateo-platformops/plumbing/crdgen/schemas"
)

const (
	apiVersionDesc = "APIVersion defines the versioned schema of this representation of an object. " +
		"Servers should convert recognized schemas to the latest internal value, and may reject " +
		"unrecognized values. More info: " +
		"https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources"
	kindDesc = "Kind is a string value representing the REST resource this object represents. " +
		"Servers may infer this from the endpoint the client submits requests to. Cannot be updated. " +
		"In CamelCase. More info: " +
		"https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds"
)

// generateDirect is the direct-transpiler implementation of Generate.
func generateDirect(opts Options) ([]byte, error) {
	specSchema, err := schemas.FromJSONReader(bytes.NewReader(opts.SpecSchema))
	if err != nil {
		return nil, fmt.Errorf("parsing spec schema: %w", err)
	}

	tr := &transpiler{refs: map[string]*schemas.Type{}}
	tr.collectRefs(specSchema)

	specProps := tr.transpile((*schemas.Type)(specSchema.ObjectAsType), "spec", map[string]bool{})
	if specProps == nil {
		specProps = openObject()
	}

	var statusProps *apiextensionsv1.JSONSchemaProps
	if len(opts.StatusSchema) > 0 {
		statusSchema, err := schemas.FromJSONReader(bytes.NewReader(opts.StatusSchema))
		if err != nil {
			return nil, fmt.Errorf("parsing status schema: %w", err)
		}
		st := &transpiler{refs: map[string]*schemas.Type{}}
		st.collectRefs(statusSchema)
		statusProps = st.transpile((*schemas.Type)(statusSchema.ObjectAsType), "status", map[string]bool{})
		tr.warnings = append(tr.warnings, st.warnings...)
	}
	if statusProps == nil {
		statusProps = openObject()
	}
	if opts.Managed {
		mergeConditionedStatus(statusProps)
	}

	crd := tr.assembleCRD(opts, specProps, statusProps)

	if err := validateCRD(crd); err != nil {
		return nil, fmt.Errorf("generated CRD failed the apiserver's own validation (this is a crdgen bug, not a config error): %w", err)
	}

	out, err := yaml.Marshal(crd)
	if err != nil {
		return nil, fmt.Errorf("marshalling CRD: %w", err)
	}
	return append([]byte("---\n"), out...), nil
}

type transpiler struct {
	refs     map[string]*schemas.Type // JSON-pointer-keyed $defs table
	warnings []string
}

// collectRefs builds the $defs table keyed by full JSON pointer. It indexes $defs at EVERY nesting
// level — root, inside another $def (e.g. #/$defs/Outer/$defs/Inner), and anywhere in the property
// tree — so a $ref to a nested $def resolves correctly. Pointers are canonicalized to the "$defs"
// spelling; a $ref written with the legacy "definitions" keyword is normalized on lookup
// (normalizeRefPointer). The structural walk never follows $ref nodes (they are leaves), so it
// terminates.
func (t *transpiler) collectRefs(s *schemas.Schema) {
	if s == nil {
		return
	}
	// Root-level $defs live on the Schema wrapper, not on the root Type.
	t.indexDefs(s.Definitions, "#")
	t.indexRefs((*schemas.Type)(s.ObjectAsType), "#")
}

// indexDefs records each entry of a $defs map under base+"/$defs/<name>" and recurses into it so its
// own nested $defs are captured too.
func (t *transpiler) indexDefs(defs schemas.Definitions, base string) {
	for name, def := range defs {
		if def == nil {
			continue
		}
		ptr := base + "/$defs/" + name
		if _, seen := t.refs[ptr]; seen {
			continue
		}
		t.refs[ptr] = def
		t.indexRefs(def, ptr)
	}
}

// indexRefs walks a node's $defs and structural children, indexing every $defs block it finds under
// its JSON pointer.
func (t *transpiler) indexRefs(node *schemas.Type, base string) {
	if node == nil {
		return
	}
	// Plain-name fragment anchors ($anchor / $dynamicAnchor) are addressable as "#<name>".
	if node.Anchor != "" {
		if _, seen := t.refs["#"+node.Anchor]; !seen {
			t.refs["#"+node.Anchor] = node
		}
	}
	if node.DynamicAnchor != "" {
		if _, seen := t.refs["#"+node.DynamicAnchor]; !seen {
			t.refs["#"+node.DynamicAnchor] = node
		}
	}
	t.indexDefs(node.Definitions, base)
	for k, p := range node.Properties {
		t.indexRefs(p, base+"/properties/"+k)
	}
	for k, p := range node.PatternProperties {
		t.indexRefs(p, base+"/patternProperties/"+k)
	}
	if node.Items != nil {
		t.indexRefs(node.Items, base+"/items")
	}
	if node.AdditionalItems != nil {
		t.indexRefs(node.AdditionalItems, base+"/additionalItems")
	}
	if node.AdditionalProperties != nil && node.AdditionalProperties.Type != nil {
		t.indexRefs(node.AdditionalProperties.Type, base+"/additionalProperties")
	}
	for i, s := range node.AllOf {
		t.indexRefs(s, fmt.Sprintf("%s/allOf/%d", base, i))
	}
	for i, s := range node.AnyOf {
		t.indexRefs(s, fmt.Sprintf("%s/anyOf/%d", base, i))
	}
	for i, s := range node.OneOf {
		t.indexRefs(s, fmt.Sprintf("%s/oneOf/%d", base, i))
	}
	if node.Not != nil {
		t.indexRefs(node.Not, base+"/not")
	}
}

// normalizeRefPointer canonicalizes a $ref to the "$defs" spelling so both #/$defs/... and the
// legacy #/definitions/... resolve against the same index.
func normalizeRefPointer(ref string) string {
	return strings.ReplaceAll(ref, "/definitions/", "/$defs/")
}

func (t *transpiler) warn(path, reason string) {
	t.warnings = append(t.warnings, fmt.Sprintf("%s: %s", path, reason))
}

// transpile converts a schemas.Type node into a structural apiextensionsv1.JSONSchemaProps.
// stack holds the JSON pointers currently being resolved, for cycle detection.
func (t *transpiler) transpile(node *schemas.Type, path string, stack map[string]bool) *apiextensionsv1.JSONSchemaProps {
	if node == nil {
		return openObject()
	}

	// $ref (and the $dynamicRef/$recursiveRef static fallback): inline the target — structural
	// schemas may not contain $ref. $dynamicRef/$recursiveRef cannot be resolved by runtime scope
	// here, so we resolve them statically against the matching $dynamicAnchor/$anchor (the bootstrap
	// fallback), degrading to preserve-unknown if there is none.
	ref := node.Ref
	if ref == "" {
		ref = node.DynamicRef
	}
	if ref == "" {
		ref = node.RecursiveRef
	}
	if ref != "" {
		key := normalizeRefPointer(ref)
		target, ok := t.refs[key]
		if !ok {
			t.warn(path, "unresolved $ref "+ref+" -> open object")
			return openObject()
		}
		if stack[key] {
			// Cycle: break exactly at the recursion edge.
			t.warn(path, "cycle at "+ref+" -> preserve-unknown")
			return openObject()
		}
		stack[key] = true
		out := t.transpile(target, path, stack)
		delete(stack, key)
		if node.Description != "" && out != nil {
			out.Description = node.Description
		}
		return out
	}

	out := &apiextensionsv1.JSONSchemaProps{}
	if node.Description != "" {
		out.Description = node.Description
	}

	// allOf: deep-merge members onto the base node.
	if len(node.AllOf) > 0 {
		return t.transpileAllOf(node, path, stack)
	}

	// x-kubernetes-int-or-string: a structural int-or-string node carries no `type`.
	if node.XIntOrString {
		out.XIntOrString = true
		if node.Title != "" {
			out.Title = node.Title
		}
		return out
	}

	// Resolve the (possibly nullable) primary type. OAS 3.0 `nullable` and JSON Schema
	// type:["x","null"] both map to structural `nullable`.
	typ, nullable := primaryType(node.Type)
	if nullable || node.Nullable {
		out.Nullable = true
	}
	// Infer a type when none is declared but a value keyword implies one (const/enum/properties/
	// items/conditionals) so the node stays structural instead of degrading to an open object.
	if typ == "" {
		typ = inferType(node)
	}

	switch {
	case len(node.Properties) > 0 || (node.AdditionalProperties != nil && node.AdditionalProperties.Type != nil):
		out.Type = "object"
		t.fillObject(out, node, path, stack)

	case typ == schemas.TypeNameObject:
		// Typed object with no declared properties -> open object (accept arbitrary content).
		out.Type = "object"
		if node.AdditionalProperties != nil && node.AdditionalProperties.IsBool && node.AdditionalProperties.Bool {
			out.XPreserveUnknownFields = boolPtr(true)
		} else if node.AdditionalProperties == nil {
			out.XPreserveUnknownFields = boolPtr(true)
		} else {
			// additionalProperties:false with no properties -> empty closed object.
			out.XPreserveUnknownFields = nil
		}

	case typ == schemas.TypeNameArray:
		out.Type = "array"
		if node.Items != nil {
			out.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: t.transpile(node.Items, path+"[]", stack)}
		} else {
			out.Items = &apiextensionsv1.JSONSchemaPropsOrArray{Schema: openObject()}
		}
		copyArrayValidation(out, node)

	case typ == schemas.TypeNameString, typ == schemas.TypeNameInteger,
		typ == schemas.TypeNameNumber, typ == schemas.TypeNameBoolean:
		out.Type = typ
		out.Format = node.Format
		copyScalarValidation(out, node)

	default:
		// No usable type. Try a constraint-only union; else open object.
		if len(node.OneOf) > 0 || len(node.AnyOf) > 0 {
			if t.attachUnion(out, node, path, stack) {
				break
			}
		}
		if node.Not != nil {
			t.warn(path, "not without a typed node -> open object (constraint dropped)")
		}
		return openObject()
	}

	// Common keywords valid on any node.
	t.copyEnum(out, node, path)
	t.copyDefault(out, node, path)
	t.copyConst(out, node, path) // const -> enum (single value)
	if node.Title != "" {
		out.Title = node.Title
	}
	t.copyExample(out, node, path)
	t.passthroughExtensions(out, node) // x-kubernetes-* carried in the input schema

	// Constraint-only unions are allowed alongside a typed node.
	if len(node.OneOf) > 0 || len(node.AnyOf) > 0 {
		t.attachUnion(out, node, path, stack)
	}

	// Conditional / dependency keywords: expressed as CEL where tractable, else degraded (warned).
	t.emitDependentRequired(out, node, path)
	t.emitNot(out, node, path)
	t.emitIfThenElse(out, node, path)
	t.handleUnsupported(out, node, path)

	return out
}

// fillObject populates an object node's properties, required, and additionalProperties (map value).
func (t *transpiler) fillObject(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string, stack map[string]bool) {
	if len(node.Properties) > 0 {
		out.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
		keys := make([]string, 0, len(node.Properties))
		for k := range node.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := t.transpile(node.Properties[k], path+"."+k, stack)
			if child != nil {
				out.Properties[k] = *child
			}
		}
	}
	if len(node.Required) > 0 {
		req := append([]string(nil), node.Required...)
		sort.Strings(req)
		out.Required = req
	}
	if node.AdditionalProperties != nil {
		switch {
		case node.AdditionalProperties.Type != nil:
			// structural schemas forbid `properties` and `additionalProperties` on the same node.
			if len(out.Properties) > 0 {
				// Keep the typed known fields; accept (untyped) extras rather than prune them.
				t.warn(path, "properties + additionalProperties schema -> keep properties + preserve-unknown")
				out.XPreserveUnknownFields = boolPtr(true)
			} else {
				// Pure map — value schema preserved (the old path collapsed this to RawExtension).
				out.AdditionalProperties = &apiextensionsv1.JSONSchemaPropsOrBool{
					Allows: true,
					Schema: t.transpile(node.AdditionalProperties.Type, path+"{}", stack),
				}
			}
		case node.AdditionalProperties.IsBool && node.AdditionalProperties.Bool:
			out.XPreserveUnknownFields = boolPtr(true)
		}
	}
}

// transpileAllOf deep-merges allOf members (and the base node's own keywords) into one object.
func (t *transpiler) transpileAllOf(node *schemas.Type, path string, stack map[string]bool) *apiextensionsv1.JSONSchemaProps {
	merged := &apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{}}
	reqSet := map[string]bool{}

	absorb := func(n *schemas.Type) {
		if n == nil {
			return
		}
		sub := t.transpile(n, path, stack)
		if sub == nil {
			return
		}
		if sub.Type != "" && sub.Type != "object" {
			// non-object member in an allOf we can't structurally merge — widen.
			merged.XPreserveUnknownFields = boolPtr(true)
			return
		}
		for k, v := range sub.Properties {
			merged.Properties[k] = v
		}
		for _, r := range sub.Required {
			reqSet[r] = true
		}
		if sub.XPreserveUnknownFields != nil && *sub.XPreserveUnknownFields {
			merged.XPreserveUnknownFields = boolPtr(true)
		}
		if merged.Description == "" && sub.Description != "" {
			merged.Description = sub.Description
		}
	}

	base := *node
	base.AllOf = nil
	absorb(&base)
	for _, m := range node.AllOf {
		absorb(m)
	}

	if len(merged.Properties) == 0 {
		merged.Properties = nil
		if merged.XPreserveUnknownFields == nil {
			merged.XPreserveUnknownFields = boolPtr(true)
		}
	}
	if len(reqSet) > 0 {
		for r := range reqSet {
			merged.Required = append(merged.Required, r)
		}
		sort.Strings(merged.Required)
	}
	if node.Description != "" {
		merged.Description = node.Description
	}
	return merged
}

// attachUnion keeps a oneOf/anyOf ONLY if every member is constraint-only (no type/properties/items)
// — the structural-legal subset. A true union of divergent shapes is degraded: dropped + the node
// widened to preserve-unknown so no field is silently pruned. Returns true if the node became a
// (widened) object as a result of an all-degraded union with no base type.
func (t *transpiler) attachUnion(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string, stack map[string]bool) bool {
	members := node.OneOf
	setter := func(v []apiextensionsv1.JSONSchemaProps) { out.OneOf = v }
	kind := "oneOf"
	if len(node.AnyOf) > 0 {
		members = node.AnyOf
		setter = func(v []apiextensionsv1.JSONSchemaProps) { out.AnyOf = v }
		kind = "anyOf"
	}

	constraintOnly := true
	for _, m := range members {
		if m == nil {
			continue
		}
		if len(m.Type) > 0 || len(m.Properties) > 0 || m.Items != nil || m.Ref != "" ||
			len(m.AllOf) > 0 || len(m.AnyOf) > 0 || len(m.OneOf) > 0 || m.AdditionalProperties != nil {
			constraintOnly = false
			break
		}
	}

	if constraintOnly {
		// Members are transpiled by the normal path, which stamps structure onto them — a member with
		// no usable type falls through to openObject() and gains `type: object` +
		// x-kubernetes-preserve-unknown-fields. Both are FORBIDDEN inside a junctor and make the
		// apiserver reject the entire CRD, so strip everything structural before emitting
		// (see junctor.go for the rule and the builder-publish incident it froze).
		var v []apiextensionsv1.JSONSchemaProps
		vacuous := false
		for _, m := range members {
			sub := junctorMemberSchema(m)
			if sub == nil {
				continue
			}
			sanitizeJunctorMember(sub) // belt-and-braces: nothing structural may survive here
			if isVacuousJunctorMember(*sub) {
				vacuous = true
			}
			v = append(v, *sub)
		}

		// A member that sanitizes to {} matches everything, which makes the whole union satisfied by
		// any value: emitting it would be a no-op wearing a constraint's clothes. Drop the union and
		// say so, rather than shipping a validation that silently never fails.
		if vacuous || len(v) < 2 {
			t.warn(path, kind+" carried no expressible constraints after removing structure forbidden inside a junctor -> union dropped")
			return false
		}

		setter(v)
		return false
	}

	t.warn(path, kind+" of divergent shapes -> degraded to preserve-unknown")
	if out.Type == "" {
		out.Type = "object"
		out.XPreserveUnknownFields = boolPtr(true)
		return true
	}
	if out.Type == "object" {
		out.XPreserveUnknownFields = boolPtr(true)
	}
	return false
}

func (t *transpiler) copyEnum(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	if len(node.Enum) == 0 || out.Type == "" || out.Type == "object" {
		return
	}
	for _, e := range node.Enum {
		raw, err := json.Marshal(e)
		if err != nil {
			t.warn(path, "undmarshalable enum value dropped")
			continue
		}
		out.Enum = append(out.Enum, apiextensionsv1.JSON{Raw: raw})
	}
}

func (t *transpiler) copyDefault(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type, path string) {
	if node.Default == nil {
		return
	}
	raw, err := json.Marshal(node.Default)
	if err != nil {
		t.warn(path, "unmarshalable default dropped")
		return
	}
	out.Default = &apiextensionsv1.JSON{Raw: raw}
}

func copyScalarValidation(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type) {
	out.Pattern = node.Pattern
	out.Minimum = node.Minimum
	out.Maximum = node.Maximum
	out.MultipleOf = node.MultipleOf
	if node.MinLength > 0 {
		v := int64(node.MinLength)
		out.MinLength = &v
	}
	if node.MaxLength > 0 {
		v := int64(node.MaxLength)
		out.MaxLength = &v
	}
}

func copyArrayValidation(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type) {
	if node.MinItems > 0 {
		v := int64(node.MinItems)
		out.MinItems = &v
	}
	if node.MaxItems > 0 {
		v := int64(node.MaxItems)
		out.MaxItems = &v
	}
	applyUniqueItems(out, node)
}

// applyUniqueItems translates a JSON-Schema `uniqueItems: true` into a form Kubernetes accepts.
// A CRD structural schema FORBIDS `uniqueItems: true` outright — the apiserver rejects it because
// the runtime complexity would be quadratic — so it cannot be passed through. We preserve the
// intent wherever it is cheaply expressible, matching the transpiler's "emit the k8s-idiomatic
// equivalent, else degrade with a warning" philosophy:
//
//   - scalar items         -> x-kubernetes-list-type: set. The native, apiserver-enforced
//     uniqueness for scalar lists (O(1) to declare, no cost budget) — the
//     idiomatic CRD replacement for uniqueItems.
//   - non-scalar + bounded -> a CEL x-kubernetes-validations uniqueness rule. Uniqueness is
//     inherently O(n^2) in CEL, so this is only emitted when maxItems bounds
//     the array — otherwise the CEL cost estimator would reject the rule (the
//     same quadratic-cost concern the uniqueItems ban exists for).
//   - otherwise            -> dropped. A working CRD that just does not enforce uniqueness on this
//     field beats one the apiserver refuses to register.
//
// `uniqueItems: false`/unset leaves out.UniqueItems at its false zero value. An explicitly-authored
// x-kubernetes-list-type is respected (uniqueItems is then redundant or the author's concern).
func applyUniqueItems(out *apiextensionsv1.JSONSchemaProps, node *schemas.Type) {
	if !node.UniqueItems {
		return
	}
	if node.XListType != nil {
		return
	}
	if isScalarItems(out) {
		setType := "set"
		out.XListType = &setType
		return
	}
	if out.MaxItems != nil {
		out.XValidations = append(out.XValidations, apiextensionsv1.ValidationRule{
			Rule:    "self.all(x, self.exists_one(y, y == x))",
			Message: "Items must be unique",
		})
	}
}

// isScalarItems reports whether an array's items are a single scalar type
// (string/integer/number/boolean), for which x-kubernetes-list-type: set is valid.
func isScalarItems(out *apiextensionsv1.JSONSchemaProps) bool {
	if out.Items == nil || out.Items.Schema == nil {
		return false
	}
	switch out.Items.Schema.Type {
	case "string", "integer", "number", "boolean":
		return true
	default:
		return false
	}
}

// primaryType returns the single structural type and whether the node is nullable
// (type given as ["T","null"]).
func primaryType(tl schemas.TypeList) (string, bool) {
	if len(tl) == 0 {
		return "", false
	}
	nullable := false
	primary := ""
	for _, t := range tl {
		if t == schemas.TypeNameNull {
			nullable = true
			continue
		}
		if primary == "" {
			primary = t
		}
	}
	return primary, nullable
}

func openObject() *apiextensionsv1.JSONSchemaProps {
	return &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: boolPtr(true)}
}

func boolPtr(b bool) *bool { return &b }

// mergeConditionedStatus injects the crossplane commonv1.ConditionedStatus shape into a managed
// resource's status (matching what the Go-struct path inlined via commonv1.ConditionedStatus).
func mergeConditionedStatus(status *apiextensionsv1.JSONSchemaProps) {
	if status.Type != "object" {
		return
	}
	if status.Properties == nil {
		status.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
	}
	status.Properties["conditions"] = apiextensionsv1.JSONSchemaProps{
		Type:        "array",
		Description: "Conditions of the resource.",
		Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{
			Type:     "object",
			Required: []string{"lastTransitionTime", "reason", "status", "type"},
			Properties: map[string]apiextensionsv1.JSONSchemaProps{
				"lastTransitionTime": {Type: "string", Format: "date-time",
					Description: "LastTransitionTime is the last time this condition transitioned from one status to another."},
				"message": {Type: "string",
					Description: "A Message containing details about this condition's last transition from one status to another, if any."},
				"reason": {Type: "string", Description: "A Reason for this condition's last transition from one status to another."},
				"status": {Type: "string", Description: "Status of this condition; is it currently True, False, or Unknown?"},
				"type":   {Type: "string", Description: "Type of this condition. At most one of each condition type may apply to a resource at any point in time."},
			},
		}},
	}
}

// assembleCRD builds the CustomResourceDefinition envelope directly from Options (never from Go
// structs / controller-gen markers).
func (t *transpiler) assembleCRD(opts Options, specProps, statusProps *apiextensionsv1.JSONSchemaProps) *apiextensionsv1.CustomResourceDefinition {
	plural := strings.ToLower(flect.Pluralize(opts.Kind))
	singular := strings.ToLower(opts.Kind)
	versionName := NormalizeVersionName(opts.Version)

	root := apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"apiVersion": {Type: "string", Description: apiVersionDesc},
			"kind":       {Type: "string", Description: kindDesc},
			"metadata":   {Type: "object"},
			"spec":       *specProps,
			"status":     *statusProps,
		},
	}

	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + opts.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: opts.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:     plural,
				Singular:   singular,
				Kind:       opts.Kind,
				ListKind:   opts.Kind + "List",
				Categories: opts.Categories,
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    versionName,
				Served:  true,
				Storage: true,
				AdditionalPrinterColumns: []apiextensionsv1.CustomResourceColumnDefinition{{
					Name:     "AGE",
					Type:     "date",
					JSONPath: ".metadata.creationTimestamp",
				}},
				Schema:       &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &root},
				Subresources: &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
			}},
		},
	}
}

// validateCRD runs the generated CRD through the API server's OWN admission validation
// (ValidateCustomResourceDefinition), converting v1 -> internal first. A non-empty error list
// means the apiserver would reject the CRD — treated as a generator bug and returned loudly.
func validateCRD(v1crd *apiextensionsv1.CustomResourceDefinition) error {
	scheme := runtime.NewScheme()
	apiextinstall.Install(scheme)

	var internal apiextensions.CustomResourceDefinition
	if err := scheme.Convert(v1crd, &internal, nil); err != nil {
		return fmt.Errorf("v1 -> internal conversion: %w", err)
	}
	// Mirror what the apiserver's CRD create strategy does in PrepareForCreate: seed
	// status.storedVersions with the storage version before validating (otherwise
	// ValidateCustomResourceDefinitionStatus rejects the empty storedVersions). The emitted
	// YAML stays clean — this is only for the validation copy.
	for _, v := range internal.Spec.Versions {
		if v.Storage {
			internal.Status.StoredVersions = []string{v.Name}
		}
	}
	if errs := validation.ValidateCustomResourceDefinition(context.TODO(), &internal); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return fmt.Errorf("%d structural error(s): %s", len(errs), strings.Join(msgs, "; "))
	}
	return nil
}
