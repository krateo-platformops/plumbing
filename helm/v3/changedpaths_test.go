package helm

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func uobj(m map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: m}
}

// plumbing#112: which FIELD changed, never its value.
func TestChangedPaths(t *testing.T) {
	base := func() map[string]interface{} {
		return map[string]interface{}{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]interface{}{"name": "d", "namespace": "ns", "resourceVersion": "1",
				"annotations": map[string]interface{}{"checksum/config": "aaa", "keep": "x"}},
			"spec": map[string]interface{}{"replicas": int64(1), "args": []interface{}{"a", "b"},
				"strategy": map[string]interface{}{"type": "RollingUpdate"}},
		}
	}
	t.Run("an annotation with a slash and a scalar spec field", func(t *testing.T) {
		after := base()
		after["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})["checksum/config"] = "bbb"
		after["spec"].(map[string]interface{})["replicas"] = int64(3)
		paths, omitted, err := changedPaths(uobj(base()), uobj(after), 5)
		if err != nil {
			t.Fatal(err)
		}
		want := "metadata.annotations[checksum/config],spec.replicas"
		if strings.Join(paths, ",") != want || omitted != 0 {
			t.Errorf("paths=%v omitted=%d, want %s", paths, omitted, want)
		}
	})
	t.Run("a removed key and a replaced array", func(t *testing.T) {
		after := base()
		delete(after["spec"].(map[string]interface{}), "strategy")
		after["spec"].(map[string]interface{})["args"] = []interface{}{"a"}
		paths, _, err := changedPaths(uobj(base()), uobj(after), 5)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(paths, ",") != "spec.args,spec.strategy" {
			t.Errorf("paths=%v, want spec.args and spec.strategy", paths)
		}
	})
	t.Run("only server-owned metadata differs: no paths", func(t *testing.T) {
		after := base()
		after["metadata"].(map[string]interface{})["resourceVersion"] = "2"
		after["metadata"].(map[string]interface{})["managedFields"] = []interface{}{map[string]interface{}{"manager": "x"}}
		after["status"] = map[string]interface{}{"readyReplicas": int64(1)}
		paths, omitted, err := changedPaths(uobj(base()), uobj(after), 5)
		if err != nil || len(paths) != 0 || omitted != 0 {
			t.Errorf("paths=%v omitted=%d err=%v, want none: semanticallyChanged would call this unchanged", paths, omitted, err)
		}
	})
	t.Run("the cap holds, with the rest counted", func(t *testing.T) {
		after := base()
		ann := after["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
		for _, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
			ann[k] = "v"
		}
		paths, omitted, err := changedPaths(uobj(base()), uobj(after), 5)
		if err != nil || len(paths) != 5 || omitted != 2 {
			t.Errorf("paths=%d omitted=%d err=%v, want 5 and 2", len(paths), omitted, err)
		}
	})
}

// A Secret's changed key is named; its value never appears in the output.
func TestChangedPathsNeverCarriesAValue(t *testing.T) {
	const secretValue = "c3VwZXItc2VjcmV0LXRva2Vu"
	before := uobj(map[string]interface{}{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{"name": "s", "namespace": "ns"},
		"data":     map[string]interface{}{"token": "b2xk"}})
	after := uobj(map[string]interface{}{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{"name": "s", "namespace": "ns"},
		"data":     map[string]interface{}{"token": secretValue}})
	paths, _, err := changedPaths(before, after, 5)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "data.token" {
		t.Errorf("paths=%v, want data.token", paths)
	}
	for _, p := range paths {
		if strings.Contains(p, secretValue) || strings.Contains(p, "b2xk") {
			t.Fatalf("a path carries a value: %q", p)
		}
	}
}
