package helm

import (
	"errors"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// strictSchema forbids additional properties, which is the configuration that makes helm's own
// `global` injection a validation failure.
const strictSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": { "replicas": { "type": "integer" } }
}`

// permissiveSchema is the same schema without the prohibition — the control for every case below.
const permissiveSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": { "replicas": { "type": "integer" } }
}`

func chartWith(name, schema string, deps ...*chart.Chart) *chart.Chart {
	c := &chart.Chart{Metadata: &chart.Metadata{Name: name, Version: "0.1.0", APIVersion: "v2"}}
	if schema != "" {
		c.Schema = []byte(schema)
	}
	for _, d := range deps {
		c.AddDependency(d)
	}
	return c
}

// THE CASE THIS EXISTS FOR, reported from the field: a chart with `additionalProperties: false`
// fails because helm injects `global`, and the author cannot prevent it from their values.
//
// Asserted on the SENTINEL rather than on the message, because the sentinel is what a caller
// branches on to answer 4xx instead of 5xx — which is the actual defect in core-provider#325, where
// the information needed to resolve it in ten seconds was present in the response and in the wrong
// shape to be read.
func TestValidateValuesSchema_GlobalInjectionIsClassifiedAsASchemaFailure(t *testing.T) {
	// global at the parent propagates into the subchart, which is where the field failure was seen.
	sub := chartWith("inner", strictSchema)
	parent := chartWith("outer", permissiveSchema, sub)

	err := validateValuesSchema(parent, map[string]any{
		"global": map[string]any{"region": "eu-west"},
		"inner":  map[string]any{"replicas": int64(2)},
	})
	if err == nil {
		t.Fatal("a subchart whose schema forbids additional properties accepted helm's injected " +
			"`global`. If helm's coalesce no longer injects global into subchart values this test " +
			"is measuring nothing, which is worse than failing")
	}
	if !errors.Is(err, helmconfig.ErrValuesSchema) {
		t.Fatalf("error does not wrap ErrValuesSchema, so a caller cannot classify it and will "+
			"still answer 5xx: %v", err)
	}

	// The advice has to name the fix, because "additional properties not allowed" suggests the
	// opposite one (add the property).
	msg := err.Error()
	if !strings.Contains(msg, "additionalProperties: false") {
		t.Errorf("the message does not name the fix (removing additionalProperties: false): %s", msg)
	}
	if !strings.Contains(msg, "inner") {
		t.Errorf("the message does not name the offending chart, so the author does not know which "+
			"values.schema.json to edit: %s", msg)
	}
}

// THE CONTROL, and without it everything above passes against a function that rejects every chart.
// A permissive schema with the same values must produce NO error — otherwise the classification has
// moved every failure into the caller's column and a 4xx is as uninformative as the 5xx was.
func TestValidateValuesSchema_PermissiveSchemaWithTheSameValuesIsClean(t *testing.T) {
	sub := chartWith("inner", permissiveSchema)
	parent := chartWith("outer", permissiveSchema, sub)

	if err := validateValuesSchema(parent, map[string]any{
		"global": map[string]any{"region": "eu-west"},
		"inner":  map[string]any{"replicas": int64(2)},
	}); err != nil {
		t.Errorf("a chart that does not forbid additional properties was reported as a schema "+
			"failure: %v", err)
	}
}

// An ORDINARY schema violation — the author's values, genuinely wrong — must also be classified,
// and must NOT get the global-specific advice, which would send them to delete a prohibition that
// is doing its job.
func TestValidateValuesSchema_AnOrdinaryViolationIsClassifiedWithoutTheGlobalAdvice(t *testing.T) {
	c := chartWith("only", permissiveSchema)

	err := validateValuesSchema(c, map[string]any{"replicas": "not-an-integer"})
	if err == nil {
		t.Fatal("a string where the schema requires an integer was not reported")
	}
	if !errors.Is(err, helmconfig.ErrValuesSchema) {
		t.Fatalf("an ordinary violation does not wrap ErrValuesSchema: %v", err)
	}
	if strings.Contains(err.Error(), "additionalProperties: false") {
		t.Errorf("an ordinary type violation was given the global-injection advice, which tells "+
			"the author to remove a prohibition that is working correctly: %s", err.Error())
	}
}

// A chart with NO schema cannot violate one. Asserted because most charts in the fleet are this
// case, and a function that errored here would break every install rather than classify anything.
func TestValidateValuesSchema_NoSchemaIsNotAFailure(t *testing.T) {
	if err := validateValuesSchema(chartWith("bare", ""), map[string]any{"anything": true}); err != nil {
		t.Errorf("a chart with no values.schema.json was reported as a schema failure: %v", err)
	}
	if err := validateValuesSchema(nil, nil); err != nil {
		t.Errorf("a nil chart must not be reported as a schema failure: %v", err)
	}
}

// THE DIFFERENTIAL. Everything above compares an expectation against a fixed input. This asserts
// that the outcome changes because of the SCHEMA and nothing else: identical values, one strict
// chart and one permissive, opposite results. Without it, a function keyed on something incidental
// in the values would satisfy both arms.
func TestValidateValuesSchema_OutcomeTurnsOnTheSchemaAlone(t *testing.T) {
	vals := map[string]any{"global": map[string]any{"region": "eu-west"}, "replicas": int64(1)}

	strictErr := validateValuesSchema(chartWith("c", strictSchema), vals)
	permissiveErr := validateValuesSchema(chartWith("c", permissiveSchema), vals)

	if (strictErr == nil) == (permissiveErr == nil) {
		t.Fatalf("the same values gave the same outcome under both schemas (strict=%v, "+
			"permissive=%v), so this is not keyed on the schema and the cases above are vacuous",
			strictErr, permissiveErr)
	}
	if strictErr == nil {
		t.Error("the STRICT schema is the one that must fail; the results are inverted")
	}
}

// An unrelated error must NOT satisfy the sentinel. The whole value of a 4xx/5xx split is that a
// genuine server fault still reads as one, and this is the arm that keeps the split honest — the
// same control TestHandler_UnrelatedFailureStillAnswers500 provides in chart-inspector.
func TestErrValuesSchema_DoesNotMatchAnUnrelatedFailure(t *testing.T) {
	for _, err := range []error{
		errors.New("dialing the registry: connection refused"),
		errors.New("install failed: no matches for kind \"X\" in version \"Y\""),
	} {
		if errors.Is(err, helmconfig.ErrValuesSchema) {
			t.Errorf("an unrelated failure matched ErrValuesSchema, so a real server fault would "+
				"be reported as the caller's invalid input: %v", err)
		}
	}
}

// THE ASYMMETRY, pinned. Measured against helm v3.20.2: CoalesceValues injects `global` into every
// DEPENDENCY's values whether or not anyone supplied one, and injects nothing at the top level.
//
// Pinned as a test rather than recorded in a comment because the advice branches on it, and the
// wrong branch is destructive in the wrong direction: a top-level author told "no value you can
// write prevents it" would delete a constraint that is working correctly. If a future helm injects
// global at the top level too, this fails and the advice must be revisited — which is the point.
func TestCoalesceInjectsGlobalIntoDependenciesOnlyNotTheTopLevel(t *testing.T) {
	sub := chartWith("inner", permissiveSchema)
	parent := chartWith("outer", permissiveSchema, sub)

	co, err := chartutil.CoalesceValues(parent, map[string]any{
		"inner": map[string]any{"replicas": int64(1)},
	})
	if err != nil {
		t.Fatalf("CoalesceValues: %v", err)
	}

	if _, ok := co["global"]; ok {
		t.Error("helm injected `global` at the TOP LEVEL with none supplied. The top-level advice " +
			"says it does not, and would now be wrong: a top-level author would be told to drop a " +
			"`global` they never wrote")
	}

	inner, ok := co["inner"].(map[string]any)
	if !ok {
		t.Fatalf("coalesced subchart values are %T, not a map — this test cannot measure the "+
			"injection", co["inner"])
	}
	if _, ok := inner["global"]; !ok {
		t.Error("helm did NOT inject `global` into the dependency's values with none supplied. The " +
			"subchart advice claims the injection is unconditional and the author cannot prevent " +
			"it; if that is no longer true, a subchart author would be told to remove a prohibition " +
			"they could instead keep")
	}
}

// A top-level `additionalProperties: false` rejecting a SUPPLIED global must get the two-option
// advice, not the "you cannot prevent it" advice.
func TestValidateValuesSchema_TopLevelGlobalGetsTheOtherAdvice(t *testing.T) {
	err := validateValuesSchema(chartWith("only", strictSchema), map[string]any{
		"global":   map[string]any{"region": "eu-west"},
		"replicas": int64(1),
	})
	if err == nil {
		t.Fatal("a top-level strict schema accepted a supplied `global`")
	}
	msg := err.Error()
	if !strings.Contains(msg, "does NOT inject") {
		t.Errorf("the top-level case did not get the top-level advice: %s", msg)
	}
	if strings.Contains(msg, "No value you can write prevents it") {
		t.Errorf("a top-level failure was told the injection is unavoidable, which would have the "+
			"author delete a working constraint when dropping one values key would do: %s", msg)
	}
}

// And the dependency case must get the unavoidable advice — asserted beside the one above because
// each is only meaningful if the other comes out differently.
func TestValidateValuesSchema_SubchartGlobalGetsTheUnavoidableAdvice(t *testing.T) {
	sub := chartWith("inner", strictSchema)
	parent := chartWith("outer", permissiveSchema, sub)

	err := validateValuesSchema(parent, map[string]any{"inner": map[string]any{"replicas": int64(1)}})
	if err == nil {
		t.Fatal("a strict SUBCHART accepted helm's injected `global` with none supplied — the " +
			"injection this advice depends on did not happen")
	}
	msg := err.Error()
	if !strings.Contains(msg, "No value you can write prevents it") {
		t.Errorf("the subchart case did not get the unavoidable advice: %s", msg)
	}
	if strings.Contains(msg, "does NOT inject") {
		t.Errorf("a subchart failure was given the top-level advice, telling the author to drop a "+
			"`global` they never supplied: %s", msg)
	}
}
