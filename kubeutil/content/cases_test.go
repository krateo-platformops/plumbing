package content

import (
	"testing"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/audit/audittest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// plumbing#149: DiffersWith agrees with every case audit declares for the authorization-bearing
// metadata (audit v0.13.0, audittest.AuthorizationMetadataCases, derived from the schema's one list),
// so the two repos can't drift. Control: Differs, which counts no labels, disagrees on at least one
// case, so the cases really exercise the labels.
func TestDiffersWithAgreesWithAuditsCases(t *testing.T) {
	keys := Keys{Annotations: audit.AuthorizationAnnotations(), Labels: audit.AuthorizationLabels()}
	cases := audittest.AuthorizationMetadataCases()
	if len(cases) == 0 {
		t.Fatal("audit declares no cases: nothing was checked")
	}
	disagreeWithoutLabels := 0
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			before := &unstructured.Unstructured{Object: c.Before}
			after := &unstructured.Unstructured{Object: c.After}
			got, err := DiffersWith(before, after, keys)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.Changed {
				t.Errorf("DiffersWith = %v, audit says changed = %v", got, c.Changed)
			}
			if old, err := Differs(before, after, keys.Annotations); err == nil && old != c.Changed {
				disagreeWithoutLabels++
			}
		})
	}
	t.Logf("%d cases checked; Differs (no labels) disagrees on %d", len(cases), disagreeWithoutLabels)
	if disagreeWithoutLabels == 0 {
		t.Error("control: Differs agrees on every case, so no case exercises a label")
	}
}
