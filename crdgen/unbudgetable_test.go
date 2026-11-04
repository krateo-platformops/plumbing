package crdgen

import (
	"strconv"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Reported by the oasgen-provider session. applyUniqueItems gates the O(n^2) CEL uniqueness rule on
// the array's OWN maxItems, but the apiserver's cost estimator multiplies by the product of EVERY
// enclosing array/map bound — and an unbounded ancestor falls back to a cardinality of up to ~10^6.
// So the rule is refused however tightly the array itself is bounded.
//
// Their measurement through the real generator, with the ancestor-bounded arm as the control:
//
//	own maxItems 2  under an UNBOUNDED ancestor  -> refused,  3.1x over the per-rule budget
//	own maxItems 8  under an UNBOUNDED ancestor  -> refused, 32.1x
//	own maxItems 8  under a BOUNDED ancestor     -> GENERATED OK   <- isolates the ancestor
//
// And because validateCRD runs the apiserver's own gate before emitting, the symptom is a CRD that
// never generates — a composition that never converges, not a chart that fails to install.

// nestedUniqueSpec renders an OUTER array of objects containing an INNER bounded array of objects
// marked uniqueItems. outerMax of 0 leaves the outer array unbounded, which is the only difference
// between the defect arm and the control arm.
func nestedUniqueSpec(outerMax int) []byte {
	inner := `"inner":{"type":"array","maxItems":8,"uniqueItems":true,` +
		`"items":{"type":"object","properties":{"k":{"type":"string"}}}}`
	bound := ""
	if outerMax > 0 {
		bound = `"maxItems":` + strconv.Itoa(outerMax) + `,`
	}
	return []byte(`{"type":"object","properties":{"outer":{"type":"array",` + bound +
		`"items":{"type":"object","properties":{` + inner + `}}}}}`)
}

func gen(t *testing.T, spec []byte) ([]byte, []string, error) {
	t.Helper()
	return GenerateWithWarnings(Options{
		Group: "example.com", Version: "v1", Kind: "Widget", SpecSchema: spec,
	})
}

// THE DEFECT: an unbounded ancestor must cause the rule to be dropped, so the CRD generates at all.
func TestUnbudgetableUniqueness_UnboundedAncestorDropsTheRule(t *testing.T) {
	crd, warnings, err := gen(t, nestedUniqueSpec(0))
	if err != nil {
		t.Fatalf("generation FAILED with an unbounded ancestor: %v\n\n"+
			"That is the defect: the rule is emitted because the INNER array is bounded, the "+
			"estimator multiplies by the unbounded OUTER array, and validateCRD (the apiserver's "+
			"own gate, run before emitting) refuses the CRD. The rule should have been dropped", err)
	}
	if strings.Contains(string(crd), uniquenessRule) {
		t.Error("the uniqueness rule survived under an unbounded ancestor; it is unbudgetable by " +
			"construction and must not be emitted")
	}

	// The author must be TOLD, or they lose a constraint silently and cannot act on it.
	joined := strings.Join(warnings, " | ")
	if !strings.Contains(joined, "uniqueness rule") {
		t.Errorf("no warning names the dropped rule; warnings were: %v", warnings)
	}
	if !strings.Contains(joined, "maxItems") {
		t.Errorf("the warning does not tell the author what to ADD to get uniqueness enforced: %v", warnings)
	}
}

// THE CONTROL that isolates the ancestor: same rule, same own-bound, differing only in whether the
// enclosing array is bounded. It must be KEPT.
//
// A pass that over-drops silently removes a constraint the author asked for and the apiserver would
// have accepted — and a guard that over-refuses is one someone switches off.
func TestUnbudgetableUniqueness_BoundedAncestorKeepsTheRule(t *testing.T) {
	crd, warnings, err := gen(t, nestedUniqueSpec(8))
	if err != nil {
		t.Fatalf("generation failed with a BOUNDED ancestor: %v", err)
	}
	if !strings.Contains(string(crd), uniquenessRule) {
		t.Error("the uniqueness rule was dropped even though every ancestor is bounded")
	}
	for _, w := range warnings {
		if strings.Contains(w, "uniqueness rule") {
			t.Errorf("warned about dropping a rule that was kept: %q", w)
		}
	}
}

// A TOP-LEVEL bounded array has no ancestor to be unbounded. This arm fails if the pass treats the
// root object as a cardinality multiplier.
func TestUnbudgetableUniqueness_TopLevelBoundedArrayKeepsTheRule(t *testing.T) {
	spec := []byte(`{"type":"object","properties":{"tags":{"type":"array","maxItems":32,` +
		`"uniqueItems":true,"items":{"type":"object","properties":{"k":{"type":"string"}}}}}}`)
	crd, _, err := gen(t, spec)
	if err != nil {
		t.Fatalf("generation failed for a top-level bounded array: %v", err)
	}
	if !strings.Contains(string(crd), uniquenessRule) {
		t.Error("a top-level bounded array lost its uniqueness rule; the root object is not a " +
			"multiplier and ancestors must start bounded")
	}
}

// A SCALAR array is unaffected: it gets x-kubernetes-list-type: set, which the apiserver enforces
// natively at no CEL cost. The pass must not disturb it, and this arm would catch a pass that
// stripped list-type or walked into the wrong branch.
func TestUnbudgetableUniqueness_ScalarArrayStillUsesListTypeSet(t *testing.T) {
	spec := []byte(`{"type":"object","properties":{"outer":{"type":"array",` +
		`"items":{"type":"object","properties":{"names":{"type":"array","uniqueItems":true,` +
		`"items":{"type":"string"}}}}}}}`)
	crd, _, err := gen(t, spec)
	if err != nil {
		t.Fatalf("generation failed for a scalar inner array under an unbounded ancestor: %v", err)
	}
	if !strings.Contains(string(crd), "x-kubernetes-list-type: set") {
		t.Error("a scalar array lost its list-type: set. That path costs no CEL budget and is " +
			"unaffected by ancestors, so the pass must leave it alone")
	}
	if strings.Contains(string(crd), uniquenessRule) {
		t.Error("a CEL rule was emitted for a SCALAR array; that should be list-type: set")
	}
}

// AN AUTHOR'S OWN RULE IS NOT OURS TO DROP. The pass matches the exact rule text AND message it
// emits, so a hand-written x-kubernetes-validations entry survives even under an unbounded ancestor
// — even though it may itself be unbudgetable. Silently deleting a constraint someone wrote is a
// worse defect than the one being fixed, and the apiserver will tell them about theirs.
//
// Exercised through the walker directly, because the transpiler has no way to inject an author's
// CEL rule from a JSON Schema input.
func TestUnbudgetableUniqueness_DoesNotTouchAnAuthorsOwnRule(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rule, msg   string
		wantDropped int
	}{
		{"our rule, our message", uniquenessRule, uniquenessMessage, 1},
		{"an author's own rule", "self.size() > 0", "must not be empty", 0},
		{"our text, a DIFFERENT message", uniquenessRule, "my own uniqueness message", 0},
		{"our message, DIFFERENT text", "self.all(x, true)", uniquenessMessage, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newArrayNodeWithRule(tc.rule, tc.msg)
			// ancestorsBounded=false is the condition under which the pass acts at all.
			got := walkDropUnbudgetable(node, false)
			if got != tc.wantDropped {
				t.Errorf("dropped %d, want %d — matching must require BOTH the rule text and the "+
					"message, or the pass deletes constraints it did not write", got, tc.wantDropped)
			}
			if tc.wantDropped == 0 && len(node.XValidations) != 1 {
				t.Errorf("the rule was removed: %+v", node.XValidations)
			}
			if tc.wantDropped == 1 && node.XValidations != nil {
				t.Errorf("XValidations should be nil once emptied, got %+v", node.XValidations)
			}
		})
	}
}

// newArrayNodeWithRule builds the minimum node the walker acts on: an array carrying exactly one
// CEL rule. The real apiextensions types, not a local stand-in — the walker matches on
// apiextensionsv1.ValidationRule fields, and a test that constructed its own type would be
// asserting about a shape the production code never sees.
func newArrayNodeWithRule(rule, msg string) *apiextensionsv1.JSONSchemaProps {
	return &apiextensionsv1.JSONSchemaProps{
		Type: "array",
		XValidations: apiextensionsv1.ValidationRules{
			{Rule: rule, Message: msg},
		},
	}
}

// mapUniqueSpec renders a MAP (additionalProperties with a value schema) whose values contain an
// inner bounded array marked uniqueItems. mapMax of 0 leaves the map unbounded, which is the only
// difference between the two arms.
//
// A map is a cardinality multiplier exactly as an array is: the apiserver's estimator multiplies by
// maxProperties, and falls back to a large constant without one.
func mapUniqueSpec(mapMax int) []byte {
	inner := `"inner":{"type":"array","maxItems":8,"uniqueItems":true,` +
		`"items":{"type":"object","properties":{"k":{"type":"string"}}}}`
	bound := ""
	if mapMax > 0 {
		bound = `"maxProperties":` + strconv.Itoa(mapMax) + `,`
	}
	return []byte(`{"type":"object","properties":{"m":{"type":"object",` + bound +
		`"additionalProperties":{"type":"object","properties":{` + inner + `}}}}}`)
}

// An UNBOUNDED MAP ancestor is unbudgetable for the same reason an unbounded array is. This arm
// exists because a mutant that deleted the map term in walkDropUnbudgetable SURVIVED: nothing in
// the suite descended through additionalProperties.
func TestUnbudgetableUniqueness_UnboundedMapAncestorDropsTheRule(t *testing.T) {
	crd, warnings, err := gen(t, mapUniqueSpec(0))
	if err != nil {
		t.Fatalf("generation FAILED under an unbounded map ancestor: %v", err)
	}
	if strings.Contains(string(crd), uniquenessRule) {
		t.Error("the uniqueness rule survived under an unbounded MAP ancestor; a map multiplies " +
			"cardinality by its maxProperties and has no bound here")
	}
	if !strings.Contains(strings.Join(warnings, " | "), "uniqueness rule") {
		t.Errorf("no warning names the dropped rule: %v", warnings)
	}
}

// THE CONTROL for the map arm, and it is only constructible because maxProperties is now emitted.
// Before copyObjectValidation existed, a map was unbounded in the OUTPUT however the author bounded
// it in the input — so this arm would have failed for a reason that had nothing to do with the
// walker.
func TestUnbudgetableUniqueness_BoundedMapAncestorKeepsTheRule(t *testing.T) {
	crd, warnings, err := gen(t, mapUniqueSpec(4))
	if err != nil {
		t.Fatalf("generation failed under a BOUNDED map ancestor: %v", err)
	}
	if !strings.Contains(string(crd), uniquenessRule) {
		t.Error("the uniqueness rule was dropped under a bounded map ancestor; every enclosing " +
			"map and array is bounded, so the estimate is finite and the apiserver accepted it")
	}
	for _, w := range warnings {
		if strings.Contains(w, "uniqueness rule") {
			t.Errorf("warned about dropping a rule that was kept: %q", w)
		}
	}
}

// The passthrough on its own, separate from the uniqueness question: an author's maxProperties and
// minProperties must reach the CRD. They were parsed and discarded.
//
// This is NOT the same assertion as the control above. The control would also pass if maxProperties
// reached the walker but never the emitted YAML; this one fails in that case, and the author's lost
// constraint is the user-visible half of the defect.
func TestObjectSizeConstraintsArePropagated(t *testing.T) {
	spec := []byte(`{"type":"object","properties":{"m":{"type":"object","maxProperties":4,` +
		`"minProperties":1,"additionalProperties":{"type":"string"}}}}`)
	crd, _, err := gen(t, spec)
	if err != nil {
		t.Fatalf("generation failed: %v", err)
	}
	for _, want := range []string{"maxProperties: 4", "minProperties: 1"} {
		if !strings.Contains(string(crd), want) {
			t.Errorf("%q is absent from the emitted CRD; the author's constraint was parsed and "+
				"then dropped\n%s", want, crd)
		}
	}
}
