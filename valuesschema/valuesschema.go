// Package valuesschema infers a JSON Schema for a Helm chart that ships no values.schema.json,
// from the chart's values.yaml files alone (core-provider#568, child 1; rules decided by Diego
// 2026-11-17, D1-D8).
//
// It is a PURE function of the chart's files: no I/O beyond the fs.FS it is given, no clock, no
// cluster. The same chart bytes and the same RulesVersion give the same output bytes, so a caller
// can cache by digest and a CRD regenerated from the result does not move unless the chart did.
//
// What it is for: a consumer (core-provider, behind an explicit opt-in) turns the result into a
// typed, OPEN composition CRD and a portal form. What it is not: a replacement for an authored
// schema. It infers no constraints (no enum, bound, pattern or required), and every map it emits is
// open, so it never refuses a value the chart itself accepts. A subchart's OWN schema (D8) keeps its
// authored constraints, and its coalesced values are overlaid as defaults (overlay.go), so a
// `required` key the subchart's values.yaml fills is filled by the apiserver too, as helm fills it.
//
// RULES v1 (one table, RulesVersion):
//
//	values.yaml             inferred                                       why
//	non-empty map           {type: object, properties, additionalProperties: true}   D3 typed-open
//	{}                      {type: object}                                  free-form map
//	null / ~                {}                                              untyped (D7: no hints)
//	bool                    {type: boolean, default}                        D4 default
//	int, string             {x-kubernetes-int-or-string: true, default}      D5: "25%" and 1 both pass
//	float                   {default}                                       type: number becomes string in crdgen's caller
//	[]                      {type: array, default}
//	list of one kind        {type: array, items: <kind>, default}
//	mixed list              {type: array, items: {}, default}
//	list of maps            items = merge of every element's keys, typed-open
//	key `global`            {type: object}, at the top of EVERY chart       the cdc merges its own keys in;
//	                                                                        helm pushes the parent's down
//	dependency (name|alias) its own values.yaml merged under the parent's block, inferred open;
//	                        its own values.schema.json used instead when it has one (D8), with
//	                        local $refs rebased onto its subtree, objects that leave
//	                        additionalProperties unstated opened (helm treats them as open), and
//	                        its coalesced values overlaid as defaults
//	library chart dep       {type: object}                                  helm adds the key anyway
//	root                    open, plus x-krateo-values-schema: {source: inferred, rules}
//
// DEFAULTS (D4) are emitted on leaves only: scalars and lists. A parent map gets no `default`
// here; crdgen's materializeDefaultedParents stamps `default: {}` on every optional parent whose
// subtree holds a default (and inferred schemas declare no `required`, so every such parent is
// safe), which is what makes a leaf default fire when the whole map is omitted. One owner for that
// rule: crdgen.
//
// CLOSING (D3): a chart may close specific maps with the Chart.yaml annotation ClosedAnnotation, a
// comma-separated list of dotted value paths. It is the ONLY annotation this package reads, and it
// is not a typing hint (D7 reads none). A path that names no map is an error, so a typo cannot
// silently leave a node open.
package valuesschema

import (
	"fmt"
	"io/fs"
	"sort"
)

// RulesVersion is the version of the inference rules. It is written into every inferred schema as
// x-krateo-values-schema.rules, and it changes whenever a rule changes what is emitted for the same
// input. Within one version, rules may only loosen (core-provider#534: a regenerated schema is
// applied to live definitions only if provably looser).
const RulesVersion = "1"

// MarkerKey is the root keyword that says a schema was inferred: {source: "inferred", rules: RulesVersion}.
const MarkerKey = "x-krateo-values-schema"

// ClosedAnnotation is the Chart.yaml annotation that closes specific maps (additionalProperties:
// false). Its value is a comma-separated list of dotted paths into the values, e.g. "image,resources.limits".
const ClosedAnnotation = "krateo.io/values-schema-closed"

// FindingKind classifies one Finding, in crdgen's WarningKind style, so a consumer branches on a
// kind rather than parsing a message.
type FindingKind string

const (
	// KindUntyped: the value's type could not be inferred (null, a float, a list mixing kinds), so the
	// node accepts any value.
	KindUntyped FindingKind = "Untyped"
	// KindSubchartSchemaUsed: a dependency ships its own values.schema.json, used instead of inference (D8).
	KindSubchartSchemaUsed FindingKind = "SubchartSchemaUsed"
	// KindLibraryChart: a library-chart dependency, emitted as a free-form object.
	KindLibraryChart FindingKind = "LibraryChart"
	// KindClosedByAnnotation: a map closed by ClosedAnnotation.
	KindClosedByAnnotation FindingKind = "ClosedByAnnotation"
	// KindGlobalOpen: the `global` key at the top of any chart's values, always free-form.
	KindGlobalOpen FindingKind = "GlobalOpen"
	// KindDefaultOverlaid: a coalesced value was put on a subchart schema's node as its `default`
	// (overlay.go), so the apiserver fills what helm's coalesce fills. Detail says when it replaced
	// an authored default.
	KindDefaultOverlaid FindingKind = "DefaultOverlaid"
	// KindOverlaySkipped: a coalesced value had no schema node to go on (no such key, an open map, a
	// null, an unresolvable $ref); detail says which. Nothing was invented for it.
	KindOverlaySkipped FindingKind = "OverlaySkipped"
	// KindRefInlined: a $ref was inlined at one use site so a default could be set below it without
	// changing the shared $def; detail is the ref.
	KindRefInlined FindingKind = "RefInlined"
	// KindAuthoredMapOpened: an object in a subchart's own schema that left `additionalProperties`
	// unstated was given `additionalProperties: true`, so the CRD keeps the undeclared keys helm
	// accepts instead of pruning them (open.go); detail is the node's JSON pointer.
	KindAuthoredMapOpened FindingKind = "AuthoredMapOpened"
)

// Finding is one notable decision the inference made, at a dotted value path ("" is the root;
// list elements appear as "path[]").
type Finding struct {
	Path   string      `json:"path"`
	Kind   FindingKind `json:"kind"`
	Detail string      `json:"detail,omitempty"`
}

// Report lists every Finding, in a deterministic order (the order the values were walked).
type Report struct {
	RulesVersion string    `json:"rulesVersion"`
	Findings     []Finding `json:"findings"`
}

// Count returns how many findings have kind k.
func (r Report) Count(k FindingKind) int {
	n := 0
	for _, f := range r.Findings {
		if f.Kind == k {
			n++
		}
	}
	return n
}

// Result is an inferred schema and the report of how it was inferred.
type Result struct {
	// Schema is compact JSON, properties in values.yaml order, deterministic.
	Schema []byte
	Report Report
}

// Infer infers the values schema of the chart rooted at dir in fsys (a directory chart, its
// dependencies under charts/ as directories or .tgz archives). It ignores any root
// values.schema.json: deciding whether to infer is the caller's job.
func Infer(fsys fs.FS, dir string) (*Result, error) {
	c, err := loadChart(fsys, dir)
	if err != nil {
		return nil, err
	}
	in := &inferrer{report: Report{RulesVersion: RulesVersion}}
	closed, err := parseClosed(c.annotations[ClosedAnnotation])
	if err != nil {
		return nil, err
	}
	in.closed = closed

	root, err := in.chartNode(c, "", "#")
	if err != nil {
		return nil, err
	}
	// Sorted, so the error names the same path on every run.
	paths := make([]string, 0, len(in.closed))
	for p := range in.closed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if !in.closedUsed[p] {
			return nil, fmt.Errorf("%s names %q, which is not a map in the chart's values", ClosedAnnotation, p)
		}
	}
	root.extra = append(root.extra, kv{MarkerKey, orderedObject{
		{"source", "inferred"},
		{"rules", RulesVersion},
	}})
	b, err := root.marshal()
	if err != nil {
		return nil, err
	}
	return &Result{Schema: b, Report: in.report}, nil
}
