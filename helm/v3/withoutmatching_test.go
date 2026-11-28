package helm

import (
	"sort"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/kube"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

func testInfo(group, version, kind, name string) *resource.Info {
	gvk := schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName(name)
	u.SetNamespace("default")
	return &resource.Info{Namespace: "default", Name: name, Object: u, Mapping: &meta.RESTMapping{GroupVersionKind: gvk}}
}

func keySet(l kube.ResourceList) string {
	set := map[string]bool{}
	for _, i := range l {
		set[infoKey(i)] = true
	}
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, "|")
}

// What is left of current and target after the skipped objects leave both must delete exactly what
// the full lists would: mergeCurrent.Difference(mergeTarget) == current.Difference(target), as sets.
// Cases: an older apiVersion of a skipped object, one object listed TWICE in current, and a real
// deletion alongside.
func TestWithoutMatchingKeepsTheDeletionSetExact(t *testing.T) {
	cm := func(v, n string) *resource.Info { return testInfo("", v, "ConfigMap", n) }
	cases := map[string]struct{ current, target kube.ResourceList }{
		"older apiVersion": {kube.ResourceList{testInfo("g.io", "v1", "W", "a"), cm("v1", "b")}, kube.ResourceList{testInfo("g.io", "v2", "W", "a"), cm("v1", "b")}},
		"listed twice":     {kube.ResourceList{cm("v1", "a"), cm("v1", "a"), cm("v1", "b")}, kube.ResourceList{cm("v1", "a"), cm("v1", "b")}},
		"listed twice, one at an older version": {kube.ResourceList{testInfo("g.io", "v1", "W", "a"), testInfo("g.io", "v2", "W", "a")},
			kube.ResourceList{testInfo("g.io", "v2", "W", "a")}},
		"a deletion alongside": {kube.ResourceList{cm("v1", "a"), cm("v1", "a"), cm("v1", "gone")}, kube.ResourceList{cm("v1", "a")}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			skipped := c.target // every target is undrifted here, the most that leaves
			mergeCurrent, mergeTarget := withoutMatching(c.current, skipped), withoutMatching(c.target, skipped)
			want, got := keySet(c.current.Difference(c.target)), keySet(mergeCurrent.Difference(mergeTarget))
			if got != want {
				t.Errorf("deletions through the merge = [%s], want [%s]", got, want)
			}
			if len(mergeTarget) != 0 {
				t.Errorf("mergeTarget = %s, want empty", keySet(mergeTarget))
			}
		})
	}
}
