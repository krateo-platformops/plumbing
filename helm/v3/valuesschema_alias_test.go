package helm

import (
	"errors"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// aliasedWrapper mirrors clickhouse-operator 0.1.1: a wrapper whose dependency `upstream-op` is
// declared with `alias: operator`, and whose strict top-level schema allows only `global` and the
// ALIAS. helm renames the subchart to its alias before validating, so its defaults never appear
// under the real name.
func aliasedWrapper() *chart.Chart {
	sub := &chart.Chart{
		Metadata: &chart.Metadata{Name: "upstream-op", Version: "0.0.7", APIVersion: "v2"},
		Values:   map[string]any{"replicas": 1},
	}
	p := &chart.Chart{
		Metadata: &chart.Metadata{Name: "wrapper", Version: "0.1.1", APIVersion: "v2",
			Dependencies: []*chart.Dependency{{Name: "upstream-op", Version: "0.0.7", Alias: "operator"}}},
		Schema: []byte(`{"type":"object","additionalProperties":false,` +
			`"properties":{"global":{"type":"object"},"operator":{"type":"object"}}}`),
	}
	p.AddDependency(sub)
	return p
}

// helmVerdict is helm's OWN answer for the same chart and values: process dependencies, then render
// values with schema validation, the way install and template do. It is the oracle.
func helmVerdict(t *testing.T, vals map[string]any) error {
	t.Helper()
	c := aliasedWrapper()
	if err := chartutil.ProcessDependenciesWithMerge(c, vals); err != nil {
		t.Fatalf("helm could not process dependencies: %v", err)
	}
	_, err := chartutil.ToRenderValuesWithSchemaValidation(c, vals,
		chartutil.ReleaseOptions{Name: "x", IsInstall: true}, nil, false)
	return err
}

// THE DEFECT. The pre-check validated without processing dependencies, so an aliased subchart's
// defaults sat under its real name and a strict parent schema refused them, while helm accepted the
// same chart. Measured on clickhouse-operator 0.1.1: helm template exits 0, and the pre-check said
// "additional properties 'clickhouse-operator-helm' not allowed".
//
// A DIFFERENTIAL WITH helm AS THE ORACLE: for every input the pre-check must agree with helm. Each
// row also carries a fixed expectation, so a case where both are wrong in the same direction cannot
// pass.
func TestValidateValuesSchema_AliasedDependencyAgreesWithHelm(t *testing.T) {
	for _, tc := range []struct {
		name    string
		vals    map[string]any
		wantErr bool
	}{
		{"empty values (the field case)", map[string]any{}, false},
		{"Krateo's injected global", map[string]any{"global": map[string]any{"compositionName": "x"}}, false},
		{"values under the alias", map[string]any{"operator": map[string]any{"replicas": 3}}, false},
		{"a genuine violation is still caught (the control)", map[string]any{"unknown": 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pre := validateValuesSchema(aliasedWrapper(), tc.vals)
			oracle := helmVerdict(t, tc.vals)
			if (pre != nil) != (oracle != nil) {
				t.Fatalf("the pre-check disagrees with helm: pre-check=%v, helm=%v", pre, oracle)
			}
			if (pre != nil) != tc.wantErr {
				t.Fatalf("pre-check=%v, want error=%v", pre, tc.wantErr)
			}
			if pre != nil && !errors.Is(pre, helmconfig.ErrValuesSchema) {
				t.Errorf("a schema violation must be ErrValuesSchema, got %v", pre)
			}
		})
	}
}

// THE INSTALL MUST NOT BE AFFECTED. ProcessDependenciesWithMerge mutates the chart it is given
// (renames aliased subcharts, prunes disabled ones, folds import-values into Values). The pre-check
// runs it on a copy, so the original must be exactly as loaded, and helm's own processing of it
// afterwards, which is what the real install does, must still succeed.
func TestValidateValuesSchema_LeavesTheChartUntouched(t *testing.T) {
	c := aliasedWrapper()
	if err := validateValuesSchema(c, map[string]any{}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := c.Dependencies()[0].Name(); got != "upstream-op" {
		t.Errorf("the original subchart was renamed to %q: the pre-check processed the real chart, not a copy", got)
	}
	if c.Metadata.Dependencies[0].Enabled {
		t.Error("the original chart's dependency metadata was mutated (Enabled set)")
	}
	if len(c.Values) != 0 {
		t.Errorf("the original chart's values were mutated: %v", c.Values)
	}
	// What the real install does next, on the same object.
	if err := chartutil.ProcessDependenciesWithMerge(c, map[string]any{}); err != nil {
		t.Fatalf("helm's own dependency processing failed after the pre-check: %v", err)
	}
	if _, err := chartutil.ToRenderValuesWithSchemaValidation(c, map[string]any{},
		chartutil.ReleaseOptions{Name: "x", IsInstall: true}, nil, false); err != nil {
		t.Fatalf("helm's own render failed after the pre-check: %v", err)
	}
}
