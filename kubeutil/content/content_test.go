package content

import (
	"encoding/base64"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func vobj(apiVersion, kind, ns, name string, fields map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	for k, v := range fields {
		u.Object[k] = runtime.DeepCopyJSONValue(v)
	}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func withLabels(u *unstructured.Unstructured, l map[string]string) *unstructured.Unstructured {
	u.SetLabels(l)
	return u
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// The canonical comparison the AUDIT-4 ruling asks for (snowplow, 2026-11-11): what is compared is
// what the object grants or holds, not how the list happens to be written.
func TestContentDiffers(t *testing.T) {
	rule := func(verbs ...interface{}) map[string]interface{} {
		return map[string]interface{}{"apiGroups": []interface{}{""}, "resources": []interface{}{"pods"}, "verbs": verbs}
	}
	subj := func(name string) map[string]interface{} {
		return map[string]interface{}{"kind": "User", "apiGroup": "rbac.authorization.k8s.io", "name": name}
	}
	role := func(rules ...interface{}) *unstructured.Unstructured {
		return vobj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "r", map[string]interface{}{"rules": rules})
	}
	binding := func(subjects ...interface{}) *unstructured.Unstructured {
		return vobj("rbac.authorization.k8s.io/v1", "RoleBinding", "ns", "b", map[string]interface{}{
			"roleRef":  map[string]interface{}{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "view"},
			"subjects": subjects,
		})
	}
	secret := func(content map[string]interface{}) *unstructured.Unstructured {
		return vobj("v1", "Secret", "ns", "s", content)
	}

	for _, tc := range []struct {
		name          string
		before, after runtime.Object
		want          bool
	}{
		{"rbac: rules reordered", role(rule("get"), rule("list")), role(rule("list"), rule("get")), false},
		{"rbac: duplicate rule", role(rule("get")), role(rule("get"), rule("get")), false},
		{"rbac: verbs reordered and duplicated", role(rule("get", "list")), role(rule("list", "get", "get")), false},
		{"rbac: verb added", role(rule("get")), role(rule("get", "delete")), true},
		{"rbac: subjects reordered", binding(subj("a"), subj("b")), binding(subj("b"), subj("a")), false},
		{"rbac: duplicate subject", binding(subj("a")), binding(subj("a"), subj("a")), false},
		{"rbac: subject added", binding(subj("a")), binding(subj("a"), subj("c")), true},
		{"rbac: labels only", role(rule("get")), withLabels(role(rule("get")), map[string]string{"x": "y"}), false},
		{"rbac: all rules removed", role(rule("get")), role(), true},
		{
			"secret: stringData renders to the same data",
			secret(map[string]interface{}{"type": "Opaque", "data": map[string]interface{}{"k": b64("v")}}),
			secret(map[string]interface{}{"type": "Opaque", "stringData": map[string]interface{}{"k": "v"}}),
			false,
		},
		{
			"secret: data value changed",
			secret(map[string]interface{}{"type": "Opaque", "data": map[string]interface{}{"k": b64("v")}}),
			secret(map[string]interface{}{"type": "Opaque", "data": map[string]interface{}{"k": b64("w")}}),
			true,
		},
		{
			"secret: type changed",
			secret(map[string]interface{}{"type": "Opaque", "data": map[string]interface{}{"k": b64("v")}}),
			secret(map[string]interface{}{"type": "kubernetes.io/tls", "data": map[string]interface{}{"k": b64("v")}}),
			true,
		},
		{
			"secret: labels only",
			secret(map[string]interface{}{"type": "Opaque", "data": map[string]interface{}{"k": b64("v")}}),
			withLabels(secret(map[string]interface{}{"type": "Opaque", "data": map[string]interface{}{"k": b64("v")}}), map[string]string{"a": "b"}),
			false,
		},
		{
			"secret: immutable set",
			secret(map[string]interface{}{"type": "Opaque", "data": map[string]interface{}{"k": b64("v")}}),
			secret(map[string]interface{}{"type": "Opaque", "immutable": true, "data": map[string]interface{}{"k": b64("v")}}),
			true,
		},
		{
			"rbac: roleRef changed",
			binding(subj("a")),
			func() runtime.Object {
				u := binding(subj("a"))
				u.Object["roleRef"] = map[string]interface{}{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "edit"}
				return u
			}(),
			true,
		},
		{
			"rbac: clusterrolebinding subjects reordered",
			vobj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "crb", map[string]interface{}{"subjects": []interface{}{subj("a"), subj("b")}}),
			vobj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "crb", map[string]interface{}{"subjects": []interface{}{subj("b"), subj("a")}}),
			false,
		},
		{
			"rbac: aggregationRule selectors reordered",
			vobj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "agg", map[string]interface{}{"aggregationRule": map[string]interface{}{"clusterRoleSelectors": []interface{}{
				map[string]interface{}{"matchLabels": map[string]interface{}{"a": "true"}}, map[string]interface{}{"matchLabels": map[string]interface{}{"b": "true"}}}}}),
			vobj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "agg", map[string]interface{}{"aggregationRule": map[string]interface{}{"clusterRoleSelectors": []interface{}{
				map[string]interface{}{"matchLabels": map[string]interface{}{"b": "true"}}, map[string]interface{}{"matchLabels": map[string]interface{}{"a": "true"}}}}}),
			false,
		},
		{
			"rbac: aggregationRule selector changed",
			vobj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "agg", map[string]interface{}{"aggregationRule": map[string]interface{}{"clusterRoleSelectors": []interface{}{
				map[string]interface{}{"matchLabels": map[string]interface{}{"a": "true"}}}}}),
			vobj("rbac.authorization.k8s.io/v1", "ClusterRole", "", "agg", map[string]interface{}{"aggregationRule": map[string]interface{}{"clusterRoleSelectors": []interface{}{
				map[string]interface{}{"matchLabels": map[string]interface{}{"c": "true"}}}}}),
			true,
		},
		{
			"admission: webhooks changed",
			vobj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "", "w", map[string]interface{}{"webhooks": []interface{}{map[string]interface{}{"name": "a.example.com", "failurePolicy": "Fail"}}}),
			vobj("admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration", "", "w", map[string]interface{}{"webhooks": []interface{}{map[string]interface{}{"name": "a.example.com", "failurePolicy": "Ignore"}}}),
			true,
		},
		{
			"admission: annotations only",
			vobj("admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration", "", "w", map[string]interface{}{"webhooks": []interface{}{map[string]interface{}{"name": "a.example.com"}}}),
			func() runtime.Object {
				u := vobj("admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration", "", "w", map[string]interface{}{"webhooks": []interface{}{map[string]interface{}{"name": "a.example.com"}}})
				u.SetAnnotations(map[string]string{"x": "y"})
				return u
			}(),
			false,
		},
		{
			"configmap: data changed",
			vobj("v1", "ConfigMap", "ns", "c", map[string]interface{}{"data": map[string]interface{}{"color": "blue"}}),
			vobj("v1", "ConfigMap", "ns", "c", map[string]interface{}{"data": map[string]interface{}{"color": "red"}}),
			true,
		},
		{
			"deployment: status only",
			vobj("apps/v1", "Deployment", "ns", "d", map[string]interface{}{"spec": map[string]interface{}{"replicas": int64(1)}, "status": map[string]interface{}{"replicas": int64(0)}}),
			vobj("apps/v1", "Deployment", "ns", "d", map[string]interface{}{"spec": map[string]interface{}{"replicas": int64(1)}, "status": map[string]interface{}{"replicas": int64(1)}}),
			false,
		},
		{
			// Order matters outside RBAC: a container list is ordered.
			"deployment: list order outside rbac is a change",
			vobj("apps/v1", "Deployment", "ns", "d", map[string]interface{}{"spec": map[string]interface{}{"args": []interface{}{"a", "b"}}}),
			vobj("apps/v1", "Deployment", "ns", "d", map[string]interface{}{"spec": map[string]interface{}{"args": []interface{}{"b", "a"}}}),
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Differs(tc.before, tc.after, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("Differs = %v, want %v", got, tc.want)
			}
		})
	}
}
