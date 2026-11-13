package helm

import (
	"reflect"
	"testing"

	"helm.sh/helm/v3/pkg/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// plumbing#155: a rendered child matched (group, kind, namespace, name) to a stored one at another
// apiVersion is stale; same version is not; a child with no stored counterpart is not (it is created
// or adopted); a same-name child of another KIND is not a counterpart.
func TestStaleManifestVersions(t *testing.T) {
	sp := func(v, name string) *unstructured.Unstructured {
		return vobj("composition.krateo.io/"+v, "Sprocket", "ns", name, nil)
	}
	current := kube.ResourceList{
		vinfo(sp("v1", "moved"), "sprockets"),
		vinfo(sp("v2", "same"), "sprockets"),
		vinfo(vobj("v1", "ConfigMap", "ns", "other-kind", nil), "configmaps"),
	}
	target := kube.ResourceList{
		vinfo(sp("v2", "moved"), "sprockets"),
		vinfo(sp("v2", "same"), "sprockets"),
		vinfo(sp("v2", "new"), "sprockets"),
		vinfo(sp("v2", "other-kind"), "sprockets"),
	}
	got := staleManifestVersions(current, target)
	want := []string{"Sprocket ns/moved: composition.krateo.io/v1 -> composition.krateo.io/v2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stale = %q, want %q", got, want)
	}
	// Control: the same lists with the moved child stored at v2 have nothing stale.
	current[0] = vinfo(sp("v2", "moved"), "sprockets")
	if got := staleManifestVersions(current, target); len(got) != 0 {
		t.Errorf("control: stale = %q, want none", got)
	}
}
