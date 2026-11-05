package crdgen

// Options configures CRD generation. SpecSchema/StatusSchema are JSON Schema documents (a chart's
// values.schema.json and an optional status schema). Managed adds the crossplane ConditionedStatus
// to the status subresource.
type Options struct {
	Group        string
	Version      string
	Kind         string
	Categories   []string
	SpecSchema   []byte
	StatusSchema []byte
	Managed      bool

	// NoParentDefaults disables the `default: {}` crdgen gives an optional object whose subtree
	// carries a reachable default (core-provider#138), which makes the apiserver materialize the
	// object and apply the nested defaults when a user omits it.
	//
	// The zero value keeps that behaviour. Set it when the schema's `default`s DESCRIBE rather than
	// instruct, e.g. an OpenAPI document generated from a vendor API, where a default states what the
	// server assumes. Materializing it there turns a documented assumption into a value the
	// controller sends, and a persisted default cannot later be told apart from an authored one
	// (plumbing#84). Every stamp is reported by GenerateWithWarnings either way.
	NoParentDefaults bool
}

// Generate transpiles the Options' JSON Schemas into a Kubernetes CustomResourceDefinition (YAML).
//
// It uses the direct JSON-Schema -> structural-OpenAPI-v3 transpiler (transpile.go): $refs are
// inlined by JSON pointer, cycles broken with x-kubernetes-preserve-unknown-fields, map value
// schemas preserved, and the result is gated on the API server's own CRD validation so the output
// is always structurally valid. See crdgen/docs/ref-resolution-redesign.md.
func Generate(opts Options) ([]byte, error) {
	crd, _, err := generateDirect(opts)
	return crd, err
}

// GenerateWithWarnings is Generate plus the transpiler's warnings.
//
// WHY THIS EXISTS. The transpiler has always collected warnings — a $ref cycle broken with
// x-kubernetes-preserve-unknown-fields, a kind it could not resolve, a generated CEL rule dropped
// because an enclosing array is unbounded — and until now every one of them was appended to a slice
// that nothing ever read. The generator knew things the chart author needed and discarded them.
//
// That is the same shape as the defect the dropped-rule warning reports: a cost paid at install
// time, by a different person, in a different week, because nothing told the author at authoring
// time. A warning into a void is not a warning.
//
// Generate is kept unchanged so no consumer has to move, but a caller that can surface warnings to
// a human or a log should prefer this one. The warnings are advisory: generation has SUCCEEDED when
// err is nil, whatever they say.
func GenerateWithWarnings(opts Options) ([]byte, []string, error) {
	crd, ws, err := generateDirect(opts)
	if ws == nil {
		// nil, not empty, so "no warnings collected" is never confused with "warnings were checked".
		return crd, nil, err
	}
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.String()
	}
	return crd, out, err
}

// GenerateWithDiagnostics is GenerateWithWarnings with each warning TYPED: its path, its Kind and its
// message (core-provider#362). A caller that acts on warnings, rather than only displaying them,
// should use this and branch on Kind (or Kind.LosesEnforcement()), never on message text.
// GenerateWithWarnings returns exactly the String() of these.
func GenerateWithDiagnostics(opts Options) ([]byte, []Warning, error) {
	return generateDirect(opts)
}
