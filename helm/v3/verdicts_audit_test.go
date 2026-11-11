package helm

import (
	"testing"

	"github.com/krateo-platformops/audit"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Given the audit schema's list, a verdict and the audit record agree on whether a write changed
// content (plumbing#124): contentDiffers says yes exactly when audit.ContentSHA256 differs, on the
// annotations, the spec, and nulls at any depth.
//
// It compares the judgement, not the bytes. The two canonical forms differ on purpose in three
// places, pinned here as rows whose verdict and hash disagree, so neither drifts unnoticed:
//   - a ConfigMap's content is only data and binaryData to the hash, every top-level field to the
//     verdict (immutable, for one);
//   - the verdict compares RBAC rules and subjects as sets, the hash as written;
//   - the verdict reads a top-level empty list as absent, the hash keeps it.
//
// Secrets aren't here: the hash refuses credential targets.
func TestContentAnnotationsAgreeWithTheAuditHash(t *testing.T) {
	keys := audit.AuthorizationAnnotations()
	if len(keys) == 0 {
		t.Fatal("the schema declares no content annotation: nothing to agree on")
	}
	listed, other := keys[0], "example.com/unrelated"
	type obj struct {
		group, resource string
		u               *unstructured.Unstructured
	}
	dep := func(spec map[string]interface{}, a map[string]string) obj {
		u := vobj("apps/v1", "Deployment", "ns", "d", map[string]interface{}{"spec": spec})
		u.SetAnnotations(a)
		return obj{"apps", "deployments", u}
	}
	replicas := func(n int64) map[string]interface{} { return map[string]interface{}{"replicas": n} }
	cmap := func(content map[string]interface{}) obj {
		return obj{"", "configmaps", vobj("v1", "ConfigMap", "ns", "c", content)}
	}
	role := func(rules ...interface{}) obj {
		return obj{"rbac.authorization.k8s.io", "roles", vobj("rbac.authorization.k8s.io/v1", "Role", "ns", "r", map[string]interface{}{"rules": rules})}
	}
	rule := func(verb string) interface{} {
		return map[string]interface{}{"apiGroups": []interface{}{""}, "resources": []interface{}{"pods"}, "verbs": []interface{}{verb}}
	}
	widget := func(content map[string]interface{}) obj {
		return obj{"example.com", "widgets", vobj("example.com/v1", "Widget", "ns", "w", content)}
	}
	hash := func(t *testing.T, o obj) string {
		h, err := audit.ContentSHA256(o.group, o.resource, "", o.u)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, tc := range []struct {
		name          string
		before, after obj
		verdict       bool // contentDiffers
		hashChanged   bool // the record's content_sha256 changed; equal to verdict unless deliberate
	}{
		{"nothing changed", dep(replicas(1), map[string]string{listed: "a"}), dep(replicas(1), map[string]string{listed: "a"}), false, false},
		{"a listed annotation changed", dep(replicas(1), map[string]string{listed: "a"}), dep(replicas(1), map[string]string{listed: "b"}), true, true},
		{"a listed annotation stamped", dep(replicas(1), nil), dep(replicas(1), map[string]string{listed: "a"}), true, true},
		{"a listed annotation removed", dep(replicas(1), map[string]string{listed: "a"}), dep(replicas(1), nil), true, true},
		{"a listed annotation set to empty", dep(replicas(1), nil), dep(replicas(1), map[string]string{listed: ""}), true, true},
		{"every listed annotation stamped", dep(replicas(1), nil), dep(replicas(1), func() map[string]string {
			m := map[string]string{}
			for _, k := range keys {
				m[k] = "x"
			}
			return m
		}()), true, true},
		{"an unlisted annotation changed", dep(replicas(1), map[string]string{other: "1"}), dep(replicas(1), map[string]string{other: "2"}), false, false},
		{"the spec changed", dep(replicas(1), nil), dep(replicas(2), nil), true, true},
		{"a nested null is an absent member", dep(map[string]interface{}{"replicas": int64(1), "paused": nil}, nil), dep(replicas(1), nil), false, false},
		{"a deeply nested null is an absent member",
			dep(map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"hostname": nil, "x": "y"}}}, nil),
			dep(map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"x": "y"}}}, nil), false, false},
		{"a null list element is a value", dep(map[string]interface{}{"args": []interface{}{"a", nil}}, nil), dep(map[string]interface{}{"args": []interface{}{"a"}}, nil), true, true},
		{"a map left empty is not an absent one", dep(map[string]interface{}{"replicas": int64(1), "strategy": map[string]interface{}{"type": nil}}, nil), dep(replicas(1), nil), true, true},

		// The deliberate differences.
		{"deliberate: a ConfigMap's immutable is content to the verdict only",
			cmap(map[string]interface{}{"data": map[string]interface{}{"k": "v"}}),
			cmap(map[string]interface{}{"data": map[string]interface{}{"k": "v"}, "immutable": true}), true, false},
		{"deliberate: reordered RBAC rules are the same set to the verdict only",
			role(rule("get"), rule("list")), role(rule("list"), rule("get")), false, true},
		{"deliberate: a top-level empty list is absent to the verdict only",
			widget(map[string]interface{}{"spec": map[string]interface{}{"a": "b"}, "items": []interface{}{}}),
			widget(map[string]interface{}{"spec": map[string]interface{}{"a": "b"}}), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := contentDiffers(tc.before.u, tc.after.u, keys)
			if err != nil {
				t.Fatal(err)
			}
			hashChanged := hash(t, tc.before) != hash(t, tc.after)
			if verdict != tc.verdict {
				t.Errorf("contentDiffers = %v, want %v", verdict, tc.verdict)
			}
			if hashChanged != tc.hashChanged {
				t.Errorf("the audit hash changed = %v, want %v", hashChanged, tc.hashChanged)
			}
		})
	}
}
