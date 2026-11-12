package helm

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/content"
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

func cm(name, color string) *unstructured.Unstructured {
	return vobj("v1", "ConfigMap", "ns", name, map[string]interface{}{"data": map[string]interface{}{"color": color}})
}

// stored marks u as helm leaves an object whose write the server confirmed: refreshed, so it
// carries a resourceVersion. A rendered object, or one whose write failed, has none.
func stored(u *unstructured.Unstructured) *unstructured.Unstructured {
	u.SetResourceVersion("101")
	return u
}

// The verdict list is uncapped, names each child once with the verb sent and how it changed, and
// agrees with the counts Reconcile reports.
func withAnnotations(u *unstructured.Unstructured, a map[string]string) *unstructured.Unstructured {
	u.SetAnnotations(a)
	return u
}

// The caller's content annotations (plumbing#124) count as content, as audit.ContentSHA256 counts
// them; every other annotation stays metadata, and with no list none counts.
func TestContentAnnotations(t *testing.T) {
	const strategy, groups = "authn.krateo.io/strategy", "authn.krateo.io/groups"
	keys := []string{strategy, groups}
	sec := func(a map[string]string) runtime.Object {
		return withAnnotations(vobj("v1", "Secret", "ns", "s", map[string]interface{}{"data": map[string]interface{}{"k": b64("v")}}), a)
	}
	for _, tc := range []struct {
		name          string
		before, after runtime.Object
		keys          []string
		want          bool
	}{
		{"a listed annotation changed", sec(map[string]string{groups: "devs"}), sec(map[string]string{groups: "devs\nops"}), keys, true},
		{"a listed annotation stamped", sec(nil), sec(map[string]string{strategy: "oidc"}), keys, true},
		{"a listed annotation removed", sec(map[string]string{strategy: "oidc"}), sec(nil), keys, true},
		{"a listed annotation set to empty is a value", sec(nil), sec(map[string]string{strategy: ""}), keys, true},
		{"a listed annotation unchanged, another changed", sec(map[string]string{strategy: "oidc", "x": "1"}), sec(map[string]string{strategy: "oidc", "x": "2"}), keys, false},
		{"an unlisted annotation changed", sec(map[string]string{"x": "1"}), sec(map[string]string{"x": "2"}), keys, false},
		{"no list: a would-be listed annotation is metadata", sec(map[string]string{groups: "devs"}), sec(map[string]string{groups: "ops"}), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := content.Differs(tc.before, tc.after, tc.keys)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("content.Differs = %v, want %v", got, tc.want)
			}
		})
	}

	// Through objectVerdicts: a merge that changed only a listed annotation is specChanged, one
	// that changed only an unlisted one is metadataOnly, and an unchanged child has no verdict.
	authz := vinfo(stored(withAnnotations(cm("authz", "blue"), map[string]string{groups: "ops"})), "configmaps")
	other := vinfo(stored(withAnnotations(cm("other", "blue"), map[string]string{groups: "devs", "x": "2"})), "configmaps")
	same := vinfo(stored(withAnnotations(cm("same", "blue"), map[string]string{groups: "devs"})), "configmaps")
	live := map[string]runtime.Object{
		infoKey(authz): withAnnotations(cm("authz", "blue"), map[string]string{groups: "devs"}),
		infoKey(other): withAnnotations(cm("other", "blue"), map[string]string{groups: "devs", "x": "1"}),
		infoKey(same):  withAnnotations(cm("same", "blue"), map[string]string{groups: "devs"}),
	}
	list := kube.ResourceList{authz, other, same}
	for _, tc := range []struct {
		name string
		keys []string
		want map[string]helmconfig.ObjectChange
	}{
		{"listed", keys, map[string]helmconfig.ObjectChange{"authz": helmconfig.ChangeSpec, "other": helmconfig.ChangeMetadataOnly}},
		{"not listed", nil, map[string]helmconfig.ObjectChange{"authz": helmconfig.ChangeMetadataOnly, "other": helmconfig.ChangeMetadataOnly}},
	} {
		t.Run("verdicts, "+tc.name, func(t *testing.T) {
			got, _, _ := objectVerdicts(verdictInputs{target: list, updated: list, live: live,
				absent: map[string]bool{}, contentAnnotations: tc.keys})
			if len(got) != len(tc.want) {
				t.Fatalf("got %d verdicts %+v, want %d (the unchanged child must be absent)", len(got), got, len(tc.want))
			}
			for _, v := range got {
				if w, ok := tc.want[v.Name]; !ok || v.Change != w {
					t.Errorf("%s: change %q, want %q", v.Name, v.Change, w)
				}
			}
		})
	}
}

func TestObjectVerdicts(t *testing.T) {
	t.Run("more than ten changed children are all listed", func(t *testing.T) {
		var target kube.ResourceList
		live := map[string]runtime.Object{}
		const n = 25
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("c%02d", i)
			info := vinfo(stored(withUID(cm(name, "red"), "uid-"+name)), "configmaps")
			live[infoKey(info)] = cm(name, "blue")
			target = append(target, info)
		}
		got, complete, _ := objectVerdicts(verdictInputs{target: target, updated: target, live: live, absent: map[string]bool{}})
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
		created := vinfo(stored(withUID(cm("new", "blue"), "uid-new")), "configmaps")
		deleted := vinfo(cm("gone", "blue"), "configmaps")
		spec := vinfo(stored(cm("spec", "red")), "configmaps")
		meta := vinfo(stored(withLabels(cm("meta", "blue"), map[string]string{"team": "a"})), "configmaps")
		same := vinfo(stored(cm("same", "blue")), "configmaps")
		keptInfo := vinfo(withUID(cm("kept", "blue"), "uid-kept"), "configmaps")
		notDoneInfo := vinfo(cm("stuck", "blue"), "configmaps")
		unknownInfo := vinfo(cm("unread", "blue"), "configmaps")

		live := map[string]runtime.Object{
			infoKey(spec): cm("spec", "blue"),
			infoKey(meta): cm("meta", "blue"),
			infoKey(same): cm("same", "blue"),
		}
		got, complete, _ := objectVerdicts(verdictInputs{
			target:  kube.ResourceList{created, spec, meta, same},
			created: kube.ResourceList{created},
			updated: kube.ResourceList{spec, meta, same},
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
		info := vinfo(stored(cm("x", "red")), "configmaps")
		got, _, _ := objectVerdicts(verdictInputs{
			target: kube.ResourceList{info}, updated: kube.ResourceList{info}, live: map[string]runtime.Object{infoKey(info): cm("x", "blue")},
			absent: map[string]bool{}, force: true,
		})
		if len(got) != 1 || got[0].Verb != "update" {
			t.Fatalf("got %+v, want one verdict with verb update", got)
		}
	})

	t.Run("a failed comparison is its own verdict", func(t *testing.T) {
		info := vinfo(stored(cm("x", "red")), "configmaps")
		// No before state: the comparison cannot run, as when the live read returned nothing usable.
		got, _, _ := objectVerdicts(verdictInputs{target: kube.ResourceList{info}, updated: kube.ResourceList{info}, live: map[string]runtime.Object{}, absent: map[string]bool{}})
		if len(got) != 1 || got[0].Change != helmconfig.ChangeCompareFailed {
			t.Fatalf("got %+v, want one compareFailed verdict", got)
		}
	})

	// helm appends to Created before sending the create and to Updated whatever the patch's outcome,
	// so its lists can't tell a failed write from a done one; the refreshed object can.
	t.Run("failed and unattempted writes", func(t *testing.T) {
		createOK := vinfo(stored(withUID(cm("made", "blue"), "uid-made")), "configmaps")
		createBad := vinfo(cm("unmade", "blue"), "configmaps") // as rendered: the create failed
		patchOK := vinfo(stored(cm("patched", "red")), "configmaps")
		patchBad := vinfo(cm("refused", "red"), "configmaps")  // as rendered: the patch failed
		skipped := vinfo(cm("unreached", "red"), "configmaps") // helm stopped before it
		skippedNew := vinfo(cm("unreachednew", "red"), "configmaps")
		live := map[string]runtime.Object{
			infoKey(patchOK):  cm("patched", "blue"),
			infoKey(patchBad): withUID(cm("refused", "blue"), "uid-refused"),
			infoKey(skipped):  cm("unreached", "blue"),
		}
		got, complete, notAttempted := objectVerdicts(verdictInputs{
			target:  kube.ResourceList{createOK, createBad, patchOK, patchBad, skipped, skippedNew},
			created: kube.ResourceList{createOK, createBad},
			updated: kube.ResourceList{patchOK, patchBad},
			live:    live,
			absent:  map[string]bool{infoKey(createOK): true, infoKey(createBad): true, infoKey(skippedNew): true},
		})
		if !complete {
			t.Fatal("complete = false")
		}
		if notAttempted != 2 {
			t.Errorf("notAttempted = %d, want 2 (unreached and unreachednew)", notAttempted)
		}
		want := map[string]helmconfig.ObjectChange{
			"made": helmconfig.ChangeCreated, "unmade": helmconfig.ChangeCreateFailed,
			"patched": helmconfig.ChangeSpec, "refused": helmconfig.ChangeUpdateFailed,
		}
		if len(got) != len(want) {
			t.Fatalf("got %+v, want %d verdicts (the unattempted children have none)", got, len(want))
		}
		for _, v := range got {
			if v.Change != want[v.Name] {
				t.Errorf("%s: %q, want %q", v.Name, v.Change, want[v.Name])
			}
			if v.Name == "refused" && (v.UID != "uid-refused" || v.Verb != "patch") {
				t.Errorf("failed patch %+v: want the live uid and verb patch", v)
			}
			if v.Name == "unmade" && v.Verb != "create" {
				t.Errorf("failed create %+v: want verb create", v)
			}
		}
	})

	t.Run("a deletion key that resolves to nothing makes the list incomplete", func(t *testing.T) {
		known := vinfo(cm("known", "x"), "configmaps")
		for name, in := range map[string]verdictInputs{
			"kept":    {current: kube.ResourceList{known}, kept: []string{infoKey(known), "v1/ConfigMap/ns/missing"}},
			"notDone": {current: kube.ResourceList{known}, notDone: []string{"v1/ConfigMap/ns/missing"}},
			"unknown": {current: kube.ResourceList{known}, unknown: []string{"v1/ConfigMap/ns/missing"}},
		} {
			got, complete, _ := objectVerdicts(in)
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
