package helm

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/resource"
)

// vobj builds an unstructured object: apiVersion, kind, namespace/name, top-level content.
func vobj(apiVersion, kind, ns, name string, content map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	for k, v := range content {
		u.Object[k] = runtime.DeepCopyJSONValue(v)
	}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

// vinfo wraps obj in a resource.Info with the REST mapping helm would have used.
func vinfo(obj *unstructured.Unstructured, plural string) *resource.Info {
	gvk := obj.GroupVersionKind()
	return &resource.Info{
		Name: obj.GetName(), Namespace: obj.GetNamespace(), Object: obj,
		Mapping: &apimeta.RESTMapping{
			Resource:         schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: plural},
			GroupVersionKind: gvk,
		},
	}
}

func withUID(u *unstructured.Unstructured, uid string) *unstructured.Unstructured {
	u.SetUID(types.UID(uid))
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
			got, err := contentDiffers(tc.before, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("contentDiffers = %v, want %v", got, tc.want)
			}
		})
	}
}

func cm(name, color string) *unstructured.Unstructured {
	return vobj("v1", "ConfigMap", "ns", name, map[string]interface{}{"data": map[string]interface{}{"color": color}})
}

// The verdict list is uncapped, names each child once with the verb sent and how it changed, and
// agrees with the counts Reconcile reports.
func TestObjectVerdicts(t *testing.T) {
	t.Run("more than ten changed children are all listed", func(t *testing.T) {
		var target kube.ResourceList
		live := map[string]runtime.Object{}
		const n = 25
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("c%02d", i)
			info := vinfo(withUID(cm(name, "red"), "uid-"+name), "configmaps")
			live[infoKey(info)] = cm(name, "blue")
			target = append(target, info)
		}
		got, complete := objectVerdicts(verdictInputs{target: target, live: live, absent: map[string]bool{}})
		if !complete {
			t.Fatal("complete = false with every key resolvable")
		}
		if len(got) != n {
			t.Fatalf("got %d verdicts, want %d (maxChangedObjects is %d and must not apply)", len(got), n, maxChangedObjects)
		}
		for _, v := range got {
			if v.Change != helmconfig.ChangeSpec || v.Verb != "patch" || v.Resource != "configmaps" || v.UID == "" {
				t.Fatalf("verdict %+v, want specChanged patch configmaps with a uid", v)
			}
		}
	})

	t.Run("each change kind", func(t *testing.T) {
		created := vinfo(withUID(cm("new", "blue"), "uid-new"), "configmaps")
		deleted := vinfo(cm("gone", "blue"), "configmaps")
		spec := vinfo(cm("spec", "red"), "configmaps")
		meta := vinfo(withLabels(cm("meta", "blue"), map[string]string{"team": "a"}), "configmaps")
		same := vinfo(cm("same", "blue"), "configmaps")
		keptInfo := vinfo(withUID(cm("kept", "blue"), "uid-kept"), "configmaps")
		notDoneInfo := vinfo(cm("stuck", "blue"), "configmaps")
		unknownInfo := vinfo(cm("unread", "blue"), "configmaps")

		live := map[string]runtime.Object{
			infoKey(spec): cm("spec", "blue"),
			infoKey(meta): cm("meta", "blue"),
			infoKey(same): cm("same", "blue"),
		}
		got, complete := objectVerdicts(verdictInputs{
			target:  kube.ResourceList{created, spec, meta, same},
			created: kube.ResourceList{created},
			deleted: kube.ResourceList{deleted},
			current: kube.ResourceList{spec, meta, same, keptInfo, notDoneInfo, unknownInfo},
			live:    live,
			absent:  map[string]bool{infoKey(created): true},
			kept:    []string{infoKey(keptInfo)},
			notDone: []string{infoKey(notDoneInfo)},
			unknown: []string{infoKey(unknownInfo)},
		})
		if !complete {
			t.Fatal("complete = false with every key resolvable")
		}
		want := map[string]struct {
			verb   string
			change helmconfig.ObjectChange
		}{
			"new":    {"create", helmconfig.ChangeCreated},
			"gone":   {"delete", helmconfig.ChangeDeleted},
			"spec":   {"patch", helmconfig.ChangeSpec},
			"meta":   {"patch", helmconfig.ChangeMetadataOnly},
			"kept":   {"delete", helmconfig.ChangeKept},
			"stuck":  {"delete", helmconfig.ChangeDeleteNotDone},
			"unread": {"delete", helmconfig.ChangeDeleteUnknown},
		}
		if len(got) != len(want) {
			t.Fatalf("got %d verdicts %+v, want %d (the unchanged child must be absent)", len(got), got, len(want))
		}
		for _, v := range got {
			w, ok := want[v.Name]
			if !ok {
				t.Fatalf("unexpected verdict %+v", v)
			}
			if v.Verb != w.verb || v.Change != w.change {
				t.Errorf("%s: verb %q change %q, want %q %q", v.Name, v.Verb, v.Change, w.verb, w.change)
			}
		}
		for _, v := range got {
			if v.Name == "kept" && v.UID != "uid-kept" {
				t.Errorf("kept deletion lost the live uid: %+v", v)
			}
		}
	})

	t.Run("force replaces: verb update", func(t *testing.T) {
		info := vinfo(cm("x", "red"), "configmaps")
		got, _ := objectVerdicts(verdictInputs{
			target: kube.ResourceList{info}, live: map[string]runtime.Object{infoKey(info): cm("x", "blue")},
			absent: map[string]bool{}, force: true,
		})
		if len(got) != 1 || got[0].Verb != "update" {
			t.Fatalf("got %+v, want one verdict with verb update", got)
		}
	})

	t.Run("a failed comparison is its own verdict", func(t *testing.T) {
		info := vinfo(cm("x", "red"), "configmaps")
		// No before state: the comparison cannot run, as when the live read returned nothing usable.
		got, _ := objectVerdicts(verdictInputs{target: kube.ResourceList{info}, live: map[string]runtime.Object{}, absent: map[string]bool{}})
		if len(got) != 1 || got[0].Change != helmconfig.ChangeCompareFailed {
			t.Fatalf("got %+v, want one compareFailed verdict", got)
		}
	})

	t.Run("a deletion key that resolves to nothing makes the list incomplete", func(t *testing.T) {
		known := vinfo(cm("known", "x"), "configmaps")
		for name, in := range map[string]verdictInputs{
			"kept":    {current: kube.ResourceList{known}, kept: []string{infoKey(known), "v1/ConfigMap/ns/missing"}},
			"notDone": {current: kube.ResourceList{known}, notDone: []string{"v1/ConfigMap/ns/missing"}},
			"unknown": {current: kube.ResourceList{known}, unknown: []string{"v1/ConfigMap/ns/missing"}},
		} {
			got, complete := objectVerdicts(in)
			if complete {
				t.Errorf("%s: complete = true although a key resolved to nothing (%+v)", name, got)
			}
		}
	})
}

// pendingDeletions splits what helm did not delete into kept and not done, from one read each.
func TestPendingDeletions(t *testing.T) {
	keep := map[string]string{kube.ResourcePolicyAnno: kube.KeepPolicy}
	a, b, c := vinfo(cm("a", "x"), "configmaps"), vinfo(cm("b", "x"), "configmaps"), vinfo(cm("c", "x"), "configmaps")
	d, e := vinfo(cm("d", "x"), "configmaps"), vinfo(cm("e", "x"), "configmaps")
	reads := 0
	get := func(info *resource.Info) (runtime.Object, error) {
		reads++
		switch info.Name {
		case "d":
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "d")
		case "e":
			return nil, errors.New("connection refused")
		}
		u := cm(info.Name, "x")
		if info.Name == "b" {
			u.SetAnnotations(keep)
		}
		return u, nil
	}
	kept, notDone, unknown := pendingDeletions(kube.ResourceList{a, b, c, d, e}, kube.ResourceList{a}, nil, get)
	if fmt.Sprint(kept) != fmt.Sprint([]string{infoKey(b)}) || fmt.Sprint(notDone) != fmt.Sprint([]string{infoKey(c)}) ||
		fmt.Sprint(unknown) != fmt.Sprint([]string{infoKey(e)}) {
		t.Fatalf("kept %v notDone %v unknown %v, want [b] [c] [e] (d is gone, so in none)", kept, notDone, unknown)
	}
	if reads != 4 {
		t.Fatalf("%d reads, want 4: one per object not deleted", reads)
	}
}
