package valuesschema

import (
	"os"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/crdgen"
)

// The crdgen round-trip gate for inferred schemas (core-provider#568, child 2), first half: every
// fixture's inferred schema goes through crdgen with no error and with no warning that changes what
// the CRD accepts. The second half, against a real apiserver, is roundtrip_envtest_test.go.
//
// An inferred schema states no constraints, so a loss of enforcement, a narrowed acceptance or a
// default crdgen cannot carry would each mean the generated CRD and helm disagree about a value the
// chart's own values.yaml produced. The one warning allowed is KindParentDefaulted: crdgen giving a
// parent `default: {}` is the mechanism the inference relies on (package doc, DEFAULTS).

// allowedRoundTripKinds are the crdgen warnings an inferred schema may produce. Anything else fails
// the gate.
var allowedRoundTripKinds = map[crdgen.WarningKind]bool{
	crdgen.KindParentDefaulted: true,
}

// gateViolations returns the warnings the gate refuses, in crdgen's order.
func gateViolations(warns []crdgen.Warning) []crdgen.Warning {
	var out []crdgen.Warning
	for _, w := range warns {
		if !allowedRoundTripKinds[w.Kind] {
			out = append(out, w)
		}
	}
	return out
}

// sortedFixtures is fixtures' names in a stable order, so subtests and failures read the same way
// every run.
func sortedFixtures() []string {
	names := make([]string, 0, len(fixtures))
	for n := range fixtures {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func generate(t *testing.T, kind string, schema []byte) ([]byte, []crdgen.Warning) {
	t.Helper()
	out, warns, err := crdgen.GenerateWithDiagnostics(crdgen.Options{
		Group: "composition.krateo.io", Version: "v1", Kind: kind, SpecSchema: schema,
	})
	if err != nil {
		t.Fatalf("crdgen: %v", err)
	}
	return out, warns
}

// crdKind is a fixture name as a CRD kind: "kyverno-3.9.2" -> "Kyverno392".
func crdKind(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			if upper {
				r -= 'a' - 'A'
			}
			b.WriteRune(r)
			upper = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			upper = false
		default:
			upper = true
		}
	}
	return b.String()
}

func TestRoundTrip_CrdgenAcceptsEveryInferredSchemaWithoutLoss(t *testing.T) {
	for _, name := range sortedFixtures() {
		t.Run(name, func(t *testing.T) {
			r := infer(t, fixtures[name])
			_, warns := generate(t, crdKind(name), r.Schema)
			for _, w := range gateViolations(warns) {
				t.Errorf("crdgen %s at %q: %s", w.Kind, w.Path, w.Message)
			}
		})
	}
}

// CONTROL: the gate's detector fires on a schema that does lose enforcement. Without it, a
// gateViolations that dropped everything would pass the test above.
func TestRoundTrip_GateRefusesALosingSchema(t *testing.T) {
	// if/then/else on a non-object node: crdgen cannot carry it (crdgen/warning_sites_test.go).
	losing := []byte(`{"type":"object","properties":{"mode":{"type":"string","if":{"minLength":1},"then":{"maxLength":3}}}}`)
	_, warns := generate(t, "Losing", losing)
	v := gateViolations(warns)
	if len(v) == 0 {
		t.Fatalf("a schema with if/then/else produced no gate violation (warnings %+v)", warns)
	}
	if !v[0].Kind.LosesEnforcement() {
		t.Errorf("violation %+v is not a loss of enforcement", v[0])
	}
}

// Every chart under testdata is a fixture, so a chart added for a unit test cannot skip the gate.
func TestRoundTrip_EveryTestdataChartIsAFixture(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "golden" {
			continue
		}
		covered := false
		for _, dir := range fixtures {
			if dir == e.Name() || strings.HasPrefix(dir, e.Name()+"/") {
				if _, err := os.Stat(path.Join("testdata", dir, "Chart.yaml")); err == nil {
					covered = true
				}
			}
		}
		if !covered {
			t.Errorf("testdata/%s is not in fixtures: add it so it goes through the round-trip gate", e.Name())
		}
	}
}
