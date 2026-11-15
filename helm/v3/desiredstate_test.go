package helm

import (
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

func TestDesiredStateChange(t *testing.T) {
	stored := &helmconfig.Release{ChartVersion: "1.0.0", Values: map[string]interface{}{
		"k": "v", "n": float64(2), "nested": map[string]interface{}{"a": true},
	}}
	for _, tc := range []struct {
		name      string
		chart     string
		requested map[string]interface{}
		want      string
	}{
		{"the same chart and values", "1.0.0", map[string]interface{}{"k": "v", "n": float64(2), "nested": map[string]interface{}{"a": true}}, ""},
		{"an int where storage decoded a float64 is the same value", "1.0.0", map[string]interface{}{"k": "v", "n": 2, "nested": map[string]interface{}{"a": true}}, ""},
		{"no values requested: helm reuses the stored ones", "1.0.0", nil, ""},
		{"a value changed", "1.0.0", map[string]interface{}{"k": "changed", "n": 2, "nested": map[string]interface{}{"a": true}}, "values"},
		{"a nested value changed", "1.0.0", map[string]interface{}{"k": "v", "n": 2, "nested": map[string]interface{}{"a": false}}, "values"},
		{"a value removed", "1.0.0", map[string]interface{}{"k": "v", "n": 2}, "values"},
		{"the chart version", "1.0.1", map[string]interface{}{"k": "v", "n": 2, "nested": map[string]interface{}{"a": true}}, "chart version 1.0.0 -> 1.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := desiredStateChange(stored, tc.chart, tc.requested)
			if err != nil || got != tc.want {
				t.Fatalf("got (%q, %v), want %q", got, err, tc.want)
			}
		})
	}
}

// Only confirmed writes are counted and named, as on the merge path.
func TestCountConfirmedWritesCountsOnlyWrites(t *testing.T) {
	r := &helmconfig.ReconcileResult{Objects: []helmconfig.ObjectVerdict{
		{Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "created", Change: helmconfig.ChangeCreated},
		{Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "patched", Change: helmconfig.ChangeSpec},
		{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "ns", Name: "meta", Change: helmconfig.ChangeMetadataOnly},
		{Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "gone", Change: helmconfig.ChangeDeleted},
		{Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "failed", Change: helmconfig.ChangeUpdateFailed},
		{Version: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "kept", Change: helmconfig.ChangeKept},
	}}
	countConfirmedWrites(r)
	if r.Created != 1 || r.PatchedUpdated != 2 || r.Deleted != 1 || len(r.ChangedObjects) != 4 {
		t.Fatalf("created %d patched %d deleted %d names %v; want 1, 2, 1 and four names (not the failed or kept child)",
			r.Created, r.PatchedUpdated, r.Deleted, r.ChangedObjects)
	}
	for _, n := range r.ChangedObjects {
		if n == "v1/ConfigMap/ns/failed" || n == "v1/ConfigMap/ns/kept" {
			t.Fatalf("listed %s as changed", n)
		}
	}
}
