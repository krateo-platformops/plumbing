package helm

import (
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/release"
)

func recordedRelease(status release.Status, manifest string) *release.Release {
	rel := &release.Release{Name: "r", Version: 4, Manifest: manifest, Info: &release.Info{Status: status},
		Labels: map[string]string{"team": "a"}}
	markApplied(rel, true)
	return rel
}

func TestAppliedRecordLabel(t *testing.T) {
	rel := recordedRelease(release.StatusFailed, "kind: ConfigMap\n")
	if !hasAppliedRecord(rel) {
		t.Fatal("a marked revision must read as recorded")
	}
	v := rel.Labels[AppliedManifestLabel]
	if !strings.HasPrefix(v, "sha256-") || len(v) != len("sha256-")+32 || len(v) > 63 {
		t.Errorf("label value %q", v)
	}
	if rel.Labels["team"] != "a" {
		t.Error("other labels must be kept")
	}

	for name, mutate := range map[string]func(r *release.Release){
		"another manifest": func(r *release.Release) { r.Manifest += "x" },
		"another revision": func(r *release.Release) { r.Version++ },
		"another release":  func(r *release.Release) { r.Name = "s" },
		"no labels":        func(r *release.Release) { r.Labels = nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := recordedRelease(release.StatusFailed, "kind: ConfigMap\n")
			mutate(r)
			if hasAppliedRecord(r) {
				t.Error("a label that does not match must not mark the manifest")
			}
		})
	}

	// Removing it copies the map: helm shares the one it carries on.
	shared := rel.Labels
	markApplied(rel, false)
	if _, ok := rel.Labels[AppliedManifestLabel]; ok || hasAppliedRecord(rel) {
		t.Error("the label must be removed")
	}
	if _, ok := shared[AppliedManifestLabel]; !ok {
		t.Error("markApplied must not change the map it was given")
	}
	if hasAppliedRecord(nil) {
		t.Error("nil")
	}
}

func TestAppliedBaseline(t *testing.T) {
	failed := &release.Release{Name: "r", Version: 2, Manifest: "m", Info: &release.Info{Status: release.StatusFailed}}
	for _, c := range []struct {
		name      string
		rel       *release.Release
		manifest  string
		wantKnown bool
	}{
		{"deployed", &release.Release{Name: "r", Version: 1, Manifest: "d", Info: &release.Info{Status: release.StatusDeployed}}, "d", true},
		{"failed, recorded", recordedRelease(release.StatusFailed, "f"), "f", true},
		{"failed, helm's render", failed, "", false},
		{"superseded, recorded", recordedRelease(release.StatusSuperseded, "s"), "", false},
		{"pending", recordedRelease(release.StatusPendingUpgrade, "p"), "", false},
		{"no info", &release.Release{Name: "r"}, "", false},
		{"none", nil, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, known := appliedBaseline(c.rel)
			if m != c.manifest || known != c.wantKnown {
				t.Errorf("= %q, %v; want %q, %v", m, known, c.manifest, c.wantKnown)
			}
		})
	}
}
