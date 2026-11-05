package crdgen

// WarningKind classifies what a crdgen warning means for the generated CRD, so a caller can act on
// the ones that matter to it without matching message text (core-provider#362).
//
// The kinds answer one question each about the CRD against its source schema:
type WarningKind string

const (
	// KindConstraintDropped: a validation the schema states is NOT enforced, so the CRD accepts
	// values the schema rejects (an unsupported `not`, if/then/else, dependentRequired, a dropped
	// uniqueness rule, ...).
	KindConstraintDropped WarningKind = "ConstraintDropped"

	// KindStructureLoosened: a subtree is accepted with unknown fields or any shape
	// (x-kubernetes-preserve-unknown-fields, an open object), e.g. a $ref cycle broken or an
	// unresolved $ref. Also accepts values the schema rejects.
	KindStructureLoosened WarningKind = "StructureLoosened"

	// KindAcceptanceNarrowed: the CRD REFUSES values the schema accepts (a union type reduced to
	// one member, an enum value that could not be carried). The opposite of a loss of enforcement,
	// and arguably worse for an author, because valid input is rejected.
	KindAcceptanceNarrowed WarningKind = "AcceptanceNarrowed"

	// KindDefaultDropped: a `default` the schema declares is not applied.
	KindDefaultDropped WarningKind = "DefaultDropped"

	// KindParentDefaulted: crdgen gave an optional parent `default: {}` so its nested defaults apply
	// when it is omitted (plumbing#84). Nothing is lost; the apiserver stores more.
	KindParentDefaulted WarningKind = "ParentDefaulted"

	// KindConventionConflict: a convention field crdgen would declare (status.observedGeneration)
	// is already declared with another type and was kept (plumbing#82).
	KindConventionConflict WarningKind = "ConventionConflict"
)

// LosesEnforcement reports whether the CRD accepts values the source schema rejects. It is the
// question behind a "schema fully enforced" signal: true for KindConstraintDropped and
// KindStructureLoosened only.
func (k WarningKind) LosesEnforcement() bool {
	return k == KindConstraintDropped || k == KindStructureLoosened
}

// Warning is one thing crdgen could not carry faithfully, or did on the caller's behalf.
type Warning struct {
	// Path is the location in the source schema walk (e.g. "spec.image.tag"), or "" for a warning
	// about the CRD as a whole.
	Path    string
	Kind    WarningKind
	Message string
}

// String is the text GenerateWithWarnings has always returned: "path: message", or the message
// alone when there is no path. Kept byte-identical so existing callers see no change.
func (w Warning) String() string {
	if w.Path == "" {
		return w.Message
	}
	return w.Path + ": " + w.Message
}
