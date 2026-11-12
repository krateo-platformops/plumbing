package content

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	principalLabel = "authn.krateo.io/principal"
	otherLabel     = "app.kubernetes.io/part-of"
	authzAnno      = "krateo.io/authorization-example"
)

func secretWith(labels, annotations map[string]string) *unstructured.Unstructured {
	u := vobj("v1", "Secret", "ns", "s", map[string]interface{}{"type": "Opaque", "data": map[string]interface{}{"k": b64("v")}})
	if labels != nil {
		u.SetLabels(labels)
	}
	if annotations != nil {
		u.SetAnnotations(annotations)
	}
	return u
}

var withPrincipal = Keys{Labels: []string{principalLabel}}

// snowplow (2026-11-12): a declared label is content; an undeclared one is metadata.
func TestDiffersWithLabels(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after *unstructured.Unstructured
		keys          Keys
		want          bool
	}{
		{"a declared label changed", secretWith(map[string]string{principalLabel: "alice"}, nil), secretWith(map[string]string{principalLabel: "bob"}, nil), withPrincipal, true},
		{"a declared label added", secretWith(nil, nil), secretWith(map[string]string{principalLabel: "alice"}, nil), withPrincipal, true},
		{"a declared label removed", secretWith(map[string]string{principalLabel: "alice"}, nil), secretWith(nil, nil), withPrincipal, true},
		{"an undeclared label changed", secretWith(map[string]string{otherLabel: "a"}, nil), secretWith(map[string]string{otherLabel: "b"}, nil), withPrincipal, false},
		{"the declared label unchanged, an undeclared one changed", secretWith(map[string]string{principalLabel: "alice", otherLabel: "a"}, nil), secretWith(map[string]string{principalLabel: "alice", otherLabel: "b"}, nil), withPrincipal, false},
		{"control: no label keys declared, the same change is not content", secretWith(map[string]string{principalLabel: "alice"}, nil), secretWith(map[string]string{principalLabel: "bob"}, nil), Keys{}, false},
		{"a declared label and a declared annotation are both kept", secretWith(map[string]string{principalLabel: "alice"}, map[string]string{authzAnno: "x"}), secretWith(map[string]string{principalLabel: "alice"}, map[string]string{authzAnno: "y"}), Keys{Annotations: []string{authzAnno}, Labels: []string{principalLabel}}, true},
		{"data changed is content whatever the keys", secretWith(nil, nil), func() *unstructured.Unstructured {
			u := secretWith(nil, nil)
			_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{"k": b64("w")}, "data")
			return u
		}(), withPrincipal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DiffersWith(tc.before, tc.after, tc.keys)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("DiffersWith = %v, want %v", got, tc.want)
			}
		})
	}
}

// Differs is exactly DiffersWith with no labels, and Canonical exactly CanonicalWith: a differential
// over objects with annotations, labels, both and neither, declared and not, so the label support
// can't move the old answer.
func TestDiffersIsDiffersWithAnnotationsOnly(t *testing.T) {
	keys := []string{authzAnno}
	objs := []*unstructured.Unstructured{
		secretWith(nil, nil),
		secretWith(map[string]string{principalLabel: "alice"}, nil),
		secretWith(map[string]string{principalLabel: "bob", otherLabel: "x"}, nil),
		secretWith(nil, map[string]string{authzAnno: "x"}),
		secretWith(nil, map[string]string{authzAnno: "y", "other": "z"}),
		secretWith(map[string]string{otherLabel: "x"}, map[string]string{authzAnno: "x"}),
		vobj("rbac.authorization.k8s.io/v1", "Role", "ns", "r", map[string]interface{}{"rules": []interface{}{map[string]interface{}{"verbs": []interface{}{"get", "list"}}}}),
		vobj("rbac.authorization.k8s.io/v1", "Role", "ns", "r", map[string]interface{}{"rules": []interface{}{map[string]interface{}{"verbs": []interface{}{"list", "get"}}}}),
	}
	pairs := 0
	for _, a := range objs {
		ca, err := Canonical(a, keys)
		if err != nil {
			t.Fatal(err)
		}
		cw, err := CanonicalWith(a, Keys{Annotations: keys})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ca, cw) {
			t.Errorf("Canonical != CanonicalWith{Annotations} for %v", a.Object)
		}
		for _, b := range objs {
			d1, err1 := Differs(a, b, keys)
			d2, err2 := DiffersWith(a, b, Keys{Annotations: keys})
			if err1 != nil || err2 != nil || d1 != d2 {
				t.Errorf("Differs=%v,%v DiffersWith=%v,%v", d1, err1, d2, err2)
			}
			pairs++
		}
	}
	if pairs != len(objs)*len(objs) {
		t.Fatalf("compared %d pairs, want %d", pairs, len(objs)*len(objs))
	}
}
