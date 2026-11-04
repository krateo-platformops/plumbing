package helm

import (
	"fmt"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// validateValuesSchema validates coalesced values against every schema in the chart tree, keeping
// helm's typed error, and wraps a failure in helmconfig.ErrValuesSchema.
//
// WHY THIS EXISTS RATHER THAN CLASSIFYING helm's OWN ERROR. helm has the typed error
// (chartutil.JSONSchemaValidationError) and discards it before an install's caller sees it: the
// chart-level entry point concatenates failures into a strings.Builder and returns
// errors.New(sb.String()) (chartutil/jsonschema.go:107), and the render path then re-wraps that with
// %s rather than %w (chartutil/values.go:168). The chain is broken twice, so errors.As downstream of
// an install can never find it. The alternative is matching the text of a strings.Builder with no
// compatibility promise.
//
// IT RUNS BEFORE THE INSTALL AND DOES NOT REPLACE helm's OWN CHECK. helm validates again during
// render, and that second run is the authority. This one exists only to learn the KIND of failure
// while the type still exists. Validation is read-only and idempotent, so running it twice costs a
// schema walk and changes nothing.
//
// THE RENDER MUST NOT DIVERGE. chart-inspector derives a composition's RBAC from a real dry-run
// install, and that identity is the only thing making the derivation trustworthy. So this coalesces
// with chartutil.CoalesceValues -- helm's own function, the same one the render path calls at
// chartutil/values.go:161 -- rather than validating raw user values. That ordering is load-bearing
// and is the whole reason the `global` case is reachable here: CoalesceValues INJECTS the global key
// (chartutil/coalesce.go:181) and validation runs after it, so a schema with
// `additionalProperties: false` rejects helm's own injection. Validating the raw values instead
// would pass, report nothing, and leave the one case this is for as a 500.
func validateValuesSchema(chrt *chart.Chart, values map[string]any) error {
	if chrt == nil {
		return nil
	}
	if values == nil {
		values = map[string]any{}
	}

	coalesced, err := chartutil.CoalesceValues(chrt, values)
	if err != nil {
		// NOT wrapped in ErrValuesSchema. A coalesce failure is not a schema violation, and
		// mislabelling it would tell a caller their schema is wrong when their values are
		// structurally unmergeable. Returned nil so the install proceeds and reports it: helm will
		// hit the same failure with its own message, and inventing a classification here would be
		// a guess dressed as a diagnosis.
		return nil
	}

	if v := validateTree(chrt, coalesced, false); v != "" {
		return fmt.Errorf("%w: %s", helmconfig.ErrValuesSchema, v)
	}
	return nil
}

// validateTree walks the chart and its dependencies, mirroring chartutil.ValidateAgainstSchema's
// recursion but calling the TYPED single-schema entry point at each node.
//
// THE RECURSION IS COPIED DELIBERATELY. The failure measured in the field names a SUBCHART --
// "additional properties 'global' not allowed" came from a dependency that received `global` from
// its parent -- so validating only the top-level schema would miss it entirely. helm recurses at
// chartutil/jsonschema.go:101 and this mirrors it; what it does not mirror is the concatenation
// that loses the type.
//
// Returns "" for agreement, or a description naming the chart.
//
// isDependency says whether chrt is a SUBCHART, and it is threaded through because the advice
// differs by it — see describeSchemaFailure. The top-level call passes false.
func validateTree(chrt *chart.Chart, values map[string]any, isDependency bool) string {
	if chrt.Schema != nil {
		if err := chartutil.ValidateAgainstSingleSchema(values, chrt.Schema); err != nil {
			return describeSchemaFailure(chrt.Name(), err, isDependency)
		}
	}
	for _, sub := range chrt.Dependencies() {
		raw, ok := values[sub.Name()]
		if !ok || raw == nil {
			continue
		}
		subValues, ok := raw.(map[string]any)
		if !ok {
			// helm reports this as a validation failure too, but it is a values TYPE error rather
			// than a schema violation, and it is not what ErrValuesSchema is for. Left to helm.
			continue
		}
		if v := validateTree(sub, subValues, true); v != "" {
			return v
		}
	}
	return ""
}

// describeSchemaFailure turns helm's validation error into a sentence a chart author can act on.
//
// THE `global` CASE IS NAMED SPECIFICALLY, and it is TWO cases rather than one. Measured against
// helm v3.20.2 by coalescing a parent and a dependency with NO user-supplied global:
//
//	subchart values   -> [replicas global]   global INJECTED
//	top-level values  -> [inner]             global NOT injected
//
// So for a DEPENDENCY the injection is unconditional and the author genuinely cannot prevent it:
// `additionalProperties: false` on a subchart is incompatible with being a subchart, and the only
// fix is to remove the prohibition. For the TOP-LEVEL chart helm injects nothing, so a rejected
// `global` there came from supplied values — and that author has two real options, including the
// one the dependency author does not have.
//
// The distinction matters because the advice is destructive in the wrong direction: telling a
// top-level author "no value you can write prevents it, remove the prohibition" would have them
// delete a constraint that is working correctly, when removing one key from their values would do.
// core-provider#325 states the general claim ("helm injects a global key into values on every
// render"), which is true of the case it was measured from — a subchart — and not of the other.
func describeSchemaFailure(chartName string, err error, isDependency bool) string {
	msg := strings.TrimSpace(err.Error())

	if mentionsGlobalRejection(msg) {
		if isDependency {
			return fmt.Sprintf("subchart %q sets `additionalProperties: false`, and helm injects a "+
				"`global` key into every dependency's values whether or not anyone supplied one, so "+
				"the schema rejects helm's own injection. No value you can write prevents it: remove "+
				"`additionalProperties: false` from that subchart's values.schema.json. helm said: %s",
				chartName, msg)
		}
		return fmt.Sprintf("chart %q sets `additionalProperties: false` and the supplied values "+
			"contain a `global` key, which the schema therefore rejects. helm does NOT inject "+
			"`global` at the top level, so either drop `global` from the values or remove "+
			"`additionalProperties: false` from values.schema.json. helm said: %s", chartName, msg)
	}
	return fmt.Sprintf("chart %q: %s", chartName, msg)
}

// mentionsGlobalRejection is message inspection, and it is confined to choosing which ADVICE to
// print -- never to deciding whether this is a schema failure at all. That decision is made by
// ValidateAgainstSingleSchema returning an error, which is a type and not a string. If helm reworded
// its output tomorrow the classification would still be right and only the advice would degrade to
// the generic sentence, which is the correct way round for a text match to fail.
func mentionsGlobalRejection(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(l, "'global'") && strings.Contains(l, "not allowed")
}
