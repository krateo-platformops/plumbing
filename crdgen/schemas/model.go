package schemas

import (
	"bytes"
	"encoding/json"
	"fmt"
)

var (
	ErrCannotMergeTypes = fmt.Errorf("cannot merge types")
	ErrEmptyTypesList   = fmt.Errorf("types list is empty")
)

// Schema is the root schema.
type Schema struct {
	*ObjectAsType
	ID          string      `json:"$id"` // RFC draft-wright-json-schema-01, section-9.2.
	LegacyID    string      `json:"id"`  // RFC draft-wright-json-schema-00, section 4.5.
	Definitions Definitions `json:"$defs,omitempty"`
}

func (s *Schema) UnmarshalJSON(data []byte) error {
	var tmp struct {
		*ObjectAsType
		ID          string      `json:"$id"`
		LegacyID    string      `json:"id"`
		Definitions Definitions `json:"$defs,omitempty"`
	}

	if err := json.Unmarshal(data, &tmp); err != nil {
		return fmt.Errorf("failed to unmarshal schema: %w", err)
	}

	if tmp.ID == "" {
		tmp.ID = tmp.LegacyID
	}

	// Take care of legacy fields.
	var legacySchema struct {
		Definitions Definitions `json:"definitions,omitempty"`
	}

	if err := json.Unmarshal(data, &legacySchema); err != nil {
		return fmt.Errorf("failed to unmarshal schema: %w", err)
	}

	if tmp.Definitions == nil && legacySchema.Definitions != nil {
		tmp.Definitions = legacySchema.Definitions
	}

	s.ObjectAsType = tmp.ObjectAsType
	s.ID = tmp.ID
	s.LegacyID = tmp.LegacyID
	s.Definitions = tmp.Definitions

	return nil
}

type (
	ObjectAsType Type
)

// TypeList is a list of type names.
type TypeList []string

// UnmarshalJSON implements json.Unmarshaler.
func (t *TypeList) UnmarshalJSON(value []byte) error {
	if len(value) > 0 && value[0] == '[' {
		var s []string
		if err := json.Unmarshal(value, &s); err != nil {
			return fmt.Errorf("failed to unmarshal type list: %w", err)
		}

		*t = s

		return nil
	}

	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return fmt.Errorf("failed to unmarshal type list: %w", err)
	}

	if s != "" {
		*t = []string{s}
	} else {
		*t = nil
	}

	return nil
}

func (t *TypeList) Equals(b TypeList) bool {
	if t == nil {
		return false
	}

	if len(*t) != len(b) {
		return false
	}

	for i := range *t {
		if (*t)[i] != b[i] {
			return false
		}
	}

	return true
}

type AdditionalProperties struct {
	IsBool bool
	Bool   bool
	Type   *Type
}

func (ap *AdditionalProperties) IsTrue() bool {
	if ap.IsBool {
		return ap.Bool
	}

	return (ap.Type != nil)
}

func (ap *AdditionalProperties) UnmarshalJSON(data []byte) error {
	type AP AdditionalProperties // raw type, no methods

	var tmp AP

	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		tmp.IsBool = true
		tmp.Bool = b
		*ap = AdditionalProperties(tmp)
		return nil
	}

	if err := json.Unmarshal(data, &tmp.Type); err != nil {
		return err
	}

	*ap = AdditionalProperties(tmp)
	return nil
}

// Definitions hold schema definitions.
// http://json-schema.org/latest/json-schema-validation.html#rfc.section.5.26
// RFC draft-wright-json-schema-validation-00, section 5.26.
type Definitions map[string]*Type

// ValidationRule mirrors a k8s x-kubernetes-validations[] entry (a CEL rule). Used both for
// passing through rules already present in the input schema and for emitting generated ones.
type ValidationRule struct {
	Rule              string  `json:"rule"`
	Message           string  `json:"message,omitempty"`
	MessageExpression string  `json:"messageExpression,omitempty"`
	Reason            *string `json:"reason,omitempty"`
	FieldPath         string  `json:"fieldPath,omitempty"`
	OptionalOldSelf   *bool   `json:"optionalOldSelf,omitempty"`
}

type SubSchemaType string

const (
	SubSchemaTypeAllOf SubSchemaType = "allOf"
	SubSchemaTypeAnyOf SubSchemaType = "anyOf"
	SubSchemaTypeOneOf SubSchemaType = "oneOf"
	SubSchemaTypeNot   SubSchemaType = "not"
)

// Type represents a JSON Schema object type.
type Type struct {
	// RFC draft-wright-json-schema-00.
	Version string `json:"$schema,omitempty"` // Section 6.1.
	Ref     string `json:"$ref,omitempty"`    // Section 7.
	// RFC draft-wright-json-schema-validation-00, section 5.
	MultipleOf           *float64              `json:"multipleOf,omitempty"`           // Section 5.1.
	Maximum              *float64              `json:"maximum,omitempty"`              // Section 5.2.
	ExclusiveMaximum     *any                  `json:"exclusiveMaximum,omitempty"`     // Section 5.3. Changed in draft 4.
	Minimum              *float64              `json:"minimum,omitempty"`              // Section 5.4.
	ExclusiveMinimum     *any                  `json:"exclusiveMinimum,omitempty"`     // Section 5.5. Changed in draft 4.
	MaxLength            int                   `json:"maxLength,omitempty"`            // Section 5.6.
	MinLength            int                   `json:"minLength,omitempty"`            // Section 5.7.
	Pattern              string                `json:"pattern,omitempty"`              // Section 5.8.
	AdditionalItems      *Type                 `json:"additionalItems,omitempty"`      // Section 5.9.
	Items                *Type                 `json:"items,omitempty"`                // Section 5.9.
	MaxItems             int                   `json:"maxItems,omitempty"`             // Section 5.10.
	MinItems             int                   `json:"minItems,omitempty"`             // Section 5.11.
	UniqueItems          bool                  `json:"uniqueItems,omitempty"`          // Section 5.12.
	MaxProperties        int                   `json:"maxProperties,omitempty"`        // Section 5.13.
	MinProperties        int                   `json:"minProperties,omitempty"`        // Section 5.14.
	Required             []string              `json:"required,omitempty"`             // Section 5.15.
	Properties           map[string]*Type      `json:"properties,omitempty"`           // Section 5.16.
	PatternProperties    map[string]*Type      `json:"patternProperties,omitempty"`    // Section 5.17.
	AdditionalProperties *AdditionalProperties `json:"additionalProperties,omitempty"` // Section 5.18.
	Enum                 []interface{}         `json:"enum,omitempty"`                 // Section 5.20.
	Type                 TypeList              `json:"type,omitempty"`                 // Section 5.21.
	// RFC draft-bhutton-json-schema-01, section 10.
	AllOf []*Type `json:"allOf,omitempty"` // Section 10.2.1.1.
	AnyOf []*Type `json:"anyOf,omitempty"` // Section 10.2.1.2.
	OneOf []*Type `json:"oneOf,omitempty"` // Section 10.2.1.3.
	Not   *Type   `json:"not,omitempty"`   // Section 10.2.1.4.
	// RFC draft-wright-json-schema-validation-00, section 6, 7.
	Title       string `json:"title,omitempty"`       // Section 6.1.
	Description string `json:"description,omitempty"` // Section 6.1.
	Default     any    `json:"default,omitempty"`     // Section 6.2.
	Examples    any    `json:"examples,omitempty"`
	Format      string `json:"format,omitempty"` // Section 7.
	// RFC draft-wright-json-schema-hyperschema-00, section 4.
	Media          *Type  `json:"media,omitempty"`          // Section 4.3.
	BinaryEncoding string `json:"binaryEncoding,omitempty"` // Section 4.3.
	// RFC draft-handrews-json-schema-validation-02, section 6.
	DependentRequired map[string][]string `json:"dependentRequired,omitempty"` // Section 6.5.4.
	// RFC draft-handrews-json-schema-validation-02, appendix A.
	Definitions      Definitions      `json:"$defs,omitempty"`
	DependentSchemas map[string]*Type `json:"dependentSchemas,omitempty"`

	// Draft 2019-09 / 2020-12 keywords.
	Const any `json:"const,omitempty"` // draft-06
	// ConstNull is true when the schema says `"const": null`, which JSON decoding can't tell apart
	// from no const at all (Const is nil for both). Set by UnmarshalJSON (plumbing#227).
	ConstNull             bool                  `json:"-"`
	If                    *Type                 `json:"if,omitempty"`                    // draft-07
	Then                  *Type                 `json:"then,omitempty"`                  // draft-07
	Else                  *Type                 `json:"else,omitempty"`                  // draft-07
	PrefixItems           []*Type               `json:"prefixItems,omitempty"`           // 2020-12 (tuple)
	UnevaluatedProperties *AdditionalProperties `json:"unevaluatedProperties,omitempty"` // 2019-09
	UnevaluatedItems      *Type                 `json:"unevaluatedItems,omitempty"`      // 2019-09
	ContentEncoding       string                `json:"contentEncoding,omitempty"`
	ContentMediaType      string                `json:"contentMediaType,omitempty"`
	ContentSchema         *Type                 `json:"contentSchema,omitempty"`
	Anchor                string                `json:"$anchor,omitempty"`          // 2019-09
	DynamicRef            string                `json:"$dynamicRef,omitempty"`      // 2020-12
	DynamicAnchor         string                `json:"$dynamicAnchor,omitempty"`   // 2020-12
	RecursiveRef          string                `json:"$recursiveRef,omitempty"`    // 2019-09
	RecursiveAnchor       bool                  `json:"$recursiveAnchor,omitempty"` // 2019-09

	// OpenAPI v3.0 nullability (OAS 3.0 / oasgen use `nullable: true` instead of type ["x","null"]).
	Nullable bool `json:"nullable,omitempty"`

	// Kubernetes structural-schema extensions (the x-kubernetes-* keywords controller-gen emits from
	// kubebuilder markers). Passed through to the CRD; XValidations also carries generated CEL rules.
	XIntOrString      bool             `json:"x-kubernetes-int-or-string,omitempty"`
	XEmbeddedResource bool             `json:"x-kubernetes-embedded-resource,omitempty"`
	XListType         *string          `json:"x-kubernetes-list-type,omitempty"`
	XListMapKeys      []string         `json:"x-kubernetes-list-map-keys,omitempty"`
	XMapType          *string          `json:"x-kubernetes-map-type,omitempty"`
	XValidations      []ValidationRule `json:"x-kubernetes-validations,omitempty"`

	// TODO: add correct section where "readOnly" is mentioned in the spec
	//       I'm not sure which section I should put here, but I did notice in the 2020-12 validation schema changelog,
	//       under the "draft-handrews-json-schema-validation-00" item it mentions "readOnly" as having been moved
	//       from hyper-schema to validation meta-data...
	ReadOnly bool `json:"readOnly,omitempty"`

	PreserveUnknownFields *bool   `json:"x-kubernetes-preserve-unknown-fields,omitempty"`
	CrdgenIdentifierName  *string `json:"x-crdgen-identifier-name,omitempty"`

	// SubSchemaType marks the type as being a subschema type.
	subSchemaType     SubSchemaType `json:"-"`
	subSchemasCount   int           `json:"-"`
	subSchemaTypeElem bool          `json:"-"`

	// Flags.
	Dereferenced bool `json:"-"` // Marks that his type has been dereferenced.
}

func (value *Type) SetSubSchemaType(sst SubSchemaType) {
	value.subSchemaType = sst
}

func (value *Type) GetSubSchemaType() SubSchemaType {
	return value.subSchemaType
}

func (value *Type) SetSubSchemasCount(ssc int) {
	value.subSchemasCount = ssc
}

func (value *Type) GetSubSchemasCount() int {
	return value.subSchemasCount
}

func (value *Type) IsSubSchemaTypeElem() bool {
	return value.subSchemaTypeElem
}

func (value *Type) SetSubSchemaTypeElem() {
	value.subSchemaTypeElem = true
}

// UnmarshalJSON accepts booleans as schemas where `true` is equivalent to `{}`
// and `false` is equivalent to `{"not": {}}`.
func (value *Type) UnmarshalJSON(raw []byte) error {
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		if b {
			*value = Type{}
		} else {
			*value = Type{Not: &Type{}}
		}

		return nil
	}

	type rawType Type
	var tmp rawType
	if err := json.Unmarshal(raw, &tmp); err != nil {
		return fmt.Errorf("failed to unmarshal type: %w", err)
	}

	// Take care of legacy fields from older RFC versions.
	legacyObj := struct {
		// RFC draft-wright-json-schema-validation-00, section 5.
		Dependencies map[string]*Type `json:"dependencies,omitempty"`
		Definitions  Definitions      `json:"definitions,omitempty"` // Section 5.26.
	}{}
	if err := json.Unmarshal(raw, &legacyObj); err != nil {
		return fmt.Errorf("failed to unmarshal type: %w", err)
	}

	if legacyObj.Definitions != nil && tmp.Definitions == nil {
		tmp.Definitions = legacyObj.Definitions
	}

	if legacyObj.Dependencies != nil && tmp.DependentSchemas == nil {
		tmp.DependentSchemas = legacyObj.Dependencies
	}

	if tmp.Const == nil {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err == nil {
			if c, ok := keys["const"]; ok && bytes.Equal(bytes.TrimSpace(c), []byte("null")) {
				tmp.ConstNull = true
			}
		}
	}

	*value = Type(tmp)

	return nil
}

type GoJSONSchemaExtension struct {
	Type       *string  `json:"type,omitempty"`
	Identifier *string  `json:"identifier,omitempty"`
	Nillable   bool     `json:"nillable,omitempty"`
	Imports    []string `json:"imports,omitempty"`
}
