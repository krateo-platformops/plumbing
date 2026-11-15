package helm

import (
	"reflect"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

const docA = `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
  annotations:
    krateo.io/traceparent: 00-aaaa-bbbb-01
data:
  k: v
`

func rel(version, manifest string, hooks ...string) *helmconfig.Release {
	r := &helmconfig.Release{ChartVersion: version, Manifest: manifest}
	for _, h := range hooks {
		r.Hooks = append(r.Hooks, helmconfig.Hook{Manifest: h})
	}
	return r
}

func TestDesiredStateChange(t *testing.T) {
	restamped := `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
  annotations:
    krateo.io/traceparent: 00-cccc-dddd-01
    krateo.io/tracestate: vendor=x
data:
  k: v
`
	changed := `apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: default
data:
  k: changed
`
	added := docA + "---\n" + `apiVersion: v1
kind: Secret
metadata:
  name: s
  namespace: default
`
	hookV1 := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: h\n  namespace: default\ndata:\n  k: v\n"
	hookV2 := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: h\n  namespace: default\ndata:\n  k: v2\n"
	for _, tc := range []struct {
		name           string
		stored, render *helmconfig.Release
		reason         string
		keys           []string
	}{
		{"identical", rel("1.0.0", docA), rel("1.0.0", docA), "", nil},
		{"only the trace stamps differ: not desired state", rel("1.0.0", docA), rel("1.0.0", restamped), "", nil},
		{"a value changed", rel("1.0.0", docA), rel("1.0.0", changed), "rendered manifest", []string{"v1/ConfigMap/default/a"}},
		{"an object added", rel("1.0.0", docA), rel("1.0.0", added), "rendered manifest", []string{"v1/Secret/default/s"}},
		{"an object removed", rel("1.0.0", added), rel("1.0.0", docA), "rendered manifest", []string{"v1/Secret/default/s"}},
		{"the chart version alone", rel("1.0.0", docA), rel("1.0.1", docA), "chart version 1.0.0 -> 1.0.1", nil},
		{"only a hook changed", rel("1.0.0", docA, hookV1), rel("1.0.0", docA, hookV2), "rendered hooks", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, keys, err := desiredStateChange(tc.stored.Manifest, tc.stored, tc.render)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.reason || !reflect.DeepEqual(keys, tc.keys) {
				t.Fatalf("got (%q, %v), want (%q, %v)", reason, keys, tc.reason, tc.keys)
			}
		})
	}
}
