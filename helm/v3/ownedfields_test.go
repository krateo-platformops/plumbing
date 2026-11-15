package helm

import (
	"testing"

	"helm.sh/helm/v3/pkg/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/resource"
)

func ownedCM(annotations map[string]interface{}, data map[string]interface{}) *unstructured.Unstructured {
	md := map[string]interface{}{"name": "c", "namespace": "default"}
	if annotations != nil {
		md["annotations"] = annotations
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": md, "data": data,
	}}
}

func infoOf(obj runtime.Object) *resource.Info {
	u := obj.(*unstructured.Unstructured)
	return &resource.Info{Name: u.GetName(), Namespace: u.GetNamespace(), Object: obj}
}

func ownedOf(t *testing.T, stored, rendered runtime.Object) fieldMask {
	t.Helper()
	var current kube.ResourceList
	if stored != nil {
		current = kube.ResourceList{infoOf(stored)}
	}
	masks := ownedFieldMasks(current, kube.ResourceList{infoOf(rendered)})
	m, ok := masks[infoKey(infoOf(rendered))]
	if !ok {
		t.Fatal("no mask for the rendered child")
	}
	return m
}

// A field neither manifest sets is another writer's: moving it is not helm's change.
func TestSemanticallyChanged_IgnoresAFieldNoManifestSets(t *testing.T) {
	rendered := ownedCM(nil, map[string]interface{}{"k": "v"})
	owned := ownedOf(t, rendered, rendered)
	before := ownedCM(map[string]interface{}{"krateo.io/traceparent-continued": "00-a-b-01"}, map[string]interface{}{"k": "v"})
	after := ownedCM(map[string]interface{}{"krateo.io/traceparent-continued": "00-c-d-01"}, map[string]interface{}{"k": "v"})
	if changed, err := semanticallyChanged(before, after, owned); err != nil || changed {
		t.Fatalf("a foreign annotation moved: changed=%v err=%v, want false", changed, err)
	}
	// Control: with no mask (the old comparison) the same pair IS a change.
	if changed, _ := semanticallyChanged(before, after, nil); !changed {
		t.Fatal("control: the unmasked comparison must see the foreign annotation, or this case proves nothing")
	}
	if paths, _, _ := changedPaths(before, after, owned, 5); len(paths) != 0 {
		t.Fatalf("changedPaths names %v, want none", paths)
	}
}

// A field the render sets that drifted is healed by helm and is a change.
func TestSemanticallyChanged_CountsADriftOnARenderedField(t *testing.T) {
	rendered := ownedCM(nil, map[string]interface{}{"k": "v"})
	owned := ownedOf(t, rendered, rendered)
	before := ownedCM(nil, map[string]interface{}{"k": "drifted"})
	after := ownedCM(nil, map[string]interface{}{"k": "v"})
	if changed, err := semanticallyChanged(before, after, owned); err != nil || !changed {
		t.Fatalf("a healed drift on data.k: changed=%v err=%v, want true", changed, err)
	}
}

// A field the chart STOPPED setting is in the stored manifest only, and the merge removes it: that
// removal is helm's change, so the stored manifest's fields are in the mask too.
func TestSemanticallyChanged_CountsAFieldTheChartStoppedSetting(t *testing.T) {
	stored := ownedCM(map[string]interface{}{"example.io/old": "x"}, map[string]interface{}{"k": "v"})
	rendered := ownedCM(nil, map[string]interface{}{"k": "v"})
	owned := ownedOf(t, stored, rendered)
	before := ownedCM(map[string]interface{}{"example.io/old": "x"}, map[string]interface{}{"k": "v"})
	after := ownedCM(nil, map[string]interface{}{"k": "v"})
	if changed, err := semanticallyChanged(before, after, owned); err != nil || !changed {
		t.Fatalf("the merge removed a field the chart stopped setting: changed=%v err=%v, want true", changed, err)
	}
}

// A list is compared whole wherever a manifest sets it.
func TestSemanticallyChanged_ComparesASetListWhole(t *testing.T) {
	withList := func(items ...interface{}) *unstructured.Unstructured {
		u := ownedCM(nil, map[string]interface{}{"k": "v"})
		u.Object["metadata"].(map[string]interface{})["finalizers"] = items
		return u
	}
	owned := ownedOf(t, withList("a"), withList("a"))
	if changed, _ := semanticallyChanged(withList("a", "b"), withList("a"), owned); !changed {
		t.Fatal("a set list lost an item: want changed")
	}
	// A list no manifest sets (finalizers added by a controller) is not helm's.
	plain := ownedCM(nil, map[string]interface{}{"k": "v"})
	if changed, _ := semanticallyChanged(withList("x"), withList("x", "y"), ownedOf(t, plain, plain)); changed {
		t.Fatal("finalizers no manifest sets moved: want not changed")
	}
}

// A field a manifest sets as a whole value (here a scalar) is compared whole even when the live
// object holds a map there: helm's merge replaces it, so a change inside that map is helm's.
func TestSemanticallyChanged_ComparesAWholeValueFieldWholeEvenWhenLiveIsAMap(t *testing.T) {
	obj := func(x interface{}) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "example.io/v1", "kind": "Thing",
			"metadata": map[string]interface{}{"name": "t", "namespace": "default"},
			"spec":     map[string]interface{}{"x": x},
		}}
	}
	owned := ownedOf(t, obj("scalar"), obj("scalar"))
	before := obj(map[string]interface{}{"a": int64(1)})
	after := obj(map[string]interface{}{"a": int64(2)})
	if changed, err := semanticallyChanged(before, after, owned); err != nil || !changed {
		t.Fatalf("spec.x, set whole by the manifest, moved: changed=%v err=%v, want true", changed, err)
	}
}
