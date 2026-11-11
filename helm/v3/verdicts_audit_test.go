package helm

import (
	"testing"

	"github.com/krateo-platformops/audit"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Given the audit schema's list, a verdict and the audit record agree on whether a write changed
// content (plumbing#124): contentDiffers says yes exactly when audit.ContentSHA256 differs.
//
// It compares the judgement, not the bytes. The two canonical forms differ on purpose elsewhere:
// the hash refuses credential targets (Secrets) and keeps only a ConfigMap's data, while the verdict
// compares RBAC as sets. So the objects here are a kind both treat the same way, a Deployment, and
// they vary along the annotations and the spec.
func TestContentAnnotationsAgreeWithTheAuditHash(t *testing.T) {
	keys := audit.AuthorizationAnnotations()
	if len(keys) == 0 {
		t.Fatal("the schema declares no content annotation: nothing to agree on")
	}
	listed, other := keys[0], "example.com/unrelated"
	dep := func(replicas int64, a map[string]string) *unstructured.Unstructured {
		u := vobj("apps/v1", "Deployment", "ns", "d", map[string]interface{}{"spec": map[string]interface{}{"replicas": replicas}})
		u.SetAnnotations(a)
		return u
	}
	hash := func(t *testing.T, u *unstructured.Unstructured) string {
		h, err := audit.ContentSHA256("apps", "deployments", "", u)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, tc := range []struct {
		name          string
		before, after *unstructured.Unstructured
		want          bool
	}{
		{"nothing changed", dep(1, map[string]string{listed: "a"}), dep(1, map[string]string{listed: "a"}), false},
		{"a listed annotation changed", dep(1, map[string]string{listed: "a"}), dep(1, map[string]string{listed: "b"}), true},
		{"a listed annotation stamped", dep(1, nil), dep(1, map[string]string{listed: "a"}), true},
		{"a listed annotation removed", dep(1, map[string]string{listed: "a"}), dep(1, nil), true},
		{"a listed annotation set to empty", dep(1, nil), dep(1, map[string]string{listed: ""}), true},
		{"every listed annotation stamped", dep(1, nil), dep(1, func() map[string]string {
			m := map[string]string{}
			for _, k := range keys {
				m[k] = "x"
			}
			return m
		}()), true},
		{"an unlisted annotation changed", dep(1, map[string]string{other: "1"}), dep(1, map[string]string{other: "2"}), false},
		{"the spec changed", dep(1, nil), dep(2, nil), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := contentDiffers(tc.before, tc.after, keys)
			if err != nil {
				t.Fatal(err)
			}
			record := hash(t, tc.before) != hash(t, tc.after)
			if verdict != record {
				t.Fatalf("contentDiffers = %v but the audit hash changed = %v: the verdict and the record disagree", verdict, record)
			}
			if verdict != tc.want {
				t.Fatalf("contentDiffers = %v, want %v", verdict, tc.want)
			}
		})
	}
}
