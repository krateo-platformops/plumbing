package crdgen

import (
	"encoding/json"
	"strings"
	"testing"

	utiljson "k8s.io/apimachinery/pkg/util/json"
)

// asDecoded turns a Go literal into the value a request body decodes to (int64 for an integral
// number, float64 otherwise), which is what the apiserver's validator sees.
func asDecoded(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := utiljson.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// plumbing#110. A values schema's exclusiveMinimum/exclusiveMaximum is enforced on the generated
// CRD, judged by the apiserver's own validator: the bound itself is refused, values just inside are
// admitted. Both drafts: draft 4's boolean modifying minimum/maximum, and draft 6's number. Before
// the fix a draft-6 bound vanished and a draft-4 strict bound became inclusive, both with no
// warning. generateV1 fails on any warning, so a drop also shows here.
func TestExclusiveBoundsAreEnforced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schema   string
		refused  []any
		admitted []any
	}{
		{"draft 6 exclusiveMinimum", `{"type":"number","exclusiveMinimum":3}`,
			[]any{3, 2.5, -1}, []any{3.001, 4, 100}},
		{"draft 6 exclusiveMaximum", `{"type":"number","exclusiveMaximum":10}`,
			[]any{10, 11}, []any{9.999, 0, -5}},
		{"draft 4 exclusiveMinimum", `{"type":"number","minimum":3,"exclusiveMinimum":true}`,
			[]any{3, 2}, []any{3.5, 4}},
		{"draft 4 exclusiveMaximum", `{"type":"integer","maximum":10,"exclusiveMaximum":true}`,
			[]any{10, 11}, []any{9, 0}},
		// Both bounds on one side apply in draft 6, so the tighter wins, whichever it is.
		{"draft 6 strict above an inclusive minimum", `{"type":"number","minimum":3,"exclusiveMinimum":5}`,
			[]any{5, 4, 3}, []any{5.5, 6}},
		{"draft 6 strict below an inclusive minimum", `{"type":"number","minimum":5,"exclusiveMinimum":3}`,
			[]any{4.9, 4, 3}, []any{5, 6}},
		{"draft 6 equal bounds give the strict one", `{"type":"number","minimum":5,"exclusiveMinimum":5}`,
			[]any{5, 4}, []any{5.1}},
		{"draft 6 strict below an inclusive maximum", `{"type":"number","maximum":10,"exclusiveMaximum":8}`,
			[]any{8, 9, 10}, []any{7.9, 0}},
		{"draft 6 strict above an inclusive maximum", `{"type":"number","maximum":8,"exclusiveMaximum":10}`,
			[]any{8.1, 9, 10}, []any{8, 0}},
		{"draft 6 both sides", `{"type":"integer","exclusiveMinimum":0,"exclusiveMaximum":5}`,
			[]any{0, 5, -1, 6}, []any{1, 4}},
		// CONTROL: false is draft 4's default, so the inclusive bound stays inclusive.
		{"draft 4 false keeps the bound inclusive", `{"type":"number","minimum":3,"exclusiveMinimum":false}`,
			[]any{2.9}, []any{3, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crd := generateV1(t, `{"type":"object","properties":{"n":`+tc.schema+`}}`)
			if errs := structuralErrs(t, crd); len(errs) != 0 {
				t.Fatalf("the apiserver would refuse this CRD: %v", errs)
			}
			for _, v := range tc.refused {
				if errs := admit(t, crd, probe("n", asDecoded(t, v))); len(errs) == 0 {
					t.Errorf("%v was ADMITTED: the bound is not enforced", v)
				}
			}
			for _, v := range tc.admitted {
				if errs := admit(t, crd, probe("n", asDecoded(t, v))); len(errs) != 0 {
					t.Errorf("%v was refused: %v", v, errs)
				}
			}
		})
	}
}

// A strict bound inside a `not` is carried too (it was the case #109 left as "dropped"): not
// (x > 3) admits 3 and below and refuses above.
func TestExclusiveBoundInsideNotIsEnforced(t *testing.T) {
	crd := generateV1(t, `{"type":"object","properties":{"n":{"type":"number","not":{"exclusiveMinimum":3}}}}`)
	if errs := structuralErrs(t, crd); len(errs) != 0 {
		t.Fatalf("the apiserver would refuse this CRD: %v", errs)
	}
	for _, v := range []any{3.5, 4} {
		if errs := admit(t, crd, probe("n", asDecoded(t, v))); len(errs) == 0 {
			t.Errorf("%v was ADMITTED: the negated strict bound is not enforced", v)
		}
	}
	for _, v := range []any{3, 2} {
		if errs := admit(t, crd, probe("n", asDecoded(t, v))); len(errs) != 0 {
			t.Errorf("%v was refused: %v", v, errs)
		}
	}
}

// A bound crdgen cannot express is reported as a dropped constraint, never lost silently: draft 4's
// true with no bound to modify, and a value of neither draft's type.
func TestUnexpressibleExclusiveBoundIsWarned(t *testing.T) {
	for _, tc := range []struct{ name, schema, want string }{
		{"draft 4 true without minimum", `{"type":"number","exclusiveMinimum":true}`, "exclusiveMinimum: true without minimum"},
		{"draft 4 true without maximum", `{"type":"number","exclusiveMaximum":true}`, "exclusiveMaximum: true without maximum"},
		{"a string", `{"type":"number","exclusiveMinimum":"3"}`, "exclusiveMinimum of type string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, warns, err := GenerateWithDiagnostics(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe",
				SpecSchema: []byte(`{"type":"object","properties":{"n":` + tc.schema + `}}`)})
			if err != nil {
				t.Fatalf("generation refused: %v", err)
			}
			found := false
			for _, w := range warns {
				if w.Kind == KindConstraintDropped && strings.Contains(w.Message, tc.want) && strings.Contains(w.Path, "n") {
					found = true
				}
			}
			if !found {
				t.Errorf("no %s warning mentioning %q at n: %v", KindConstraintDropped, tc.want, warns)
			}
		})
	}
}
