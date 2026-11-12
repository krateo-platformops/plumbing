package content

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// plumbing#145: a nil *unstructured.Unstructured passed as runtime.Object is "no object", an error,
// on either side, never a panic.
func TestTypedNilIsNoObject(t *testing.T) {
	var nilU *unstructured.Unstructured
	obj := secretWith(nil, nil)
	for _, tc := range []struct {
		name          string
		before, after *unstructured.Unstructured
	}{{"before", nilU, obj}, {"after", obj, nilU}, {"both", nilU, nilU}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Differs(tc.before, tc.after, nil); err == nil {
				t.Error("want an error for a typed-nil object, got none")
			}
		})
	}
	// Control: two real objects compare without error.
	if _, err := Differs(obj, obj, nil); err != nil {
		t.Errorf("control: %v", err)
	}
}
