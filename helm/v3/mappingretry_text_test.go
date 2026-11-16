package helm

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// helm stringifies a NoKindMatchError on some paths, so MappingRetry reads the kind back from the
// text (#191). The texts below are written by apimachinery's own Error(), never typed by hand, so a
// change to apimachinery's format fails here rather than silently emptying the log fields.
func TestNewMappingRetry_ReadsTheKindFromAStringifiedMiss(t *testing.T) {
	nk := func(group, kind string, versions ...string) error {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: group, Kind: kind}, SearchedVersions: versions}
	}
	// helm's wording when it builds the manifest (pkg/kube, "resource mapping not found").
	helmWrap := func(err error) error {
		return fmt.Errorf("unable to build kubernetes objects from release manifest: resource mapping not found for name: %q namespace: %q from %q: %s\nensure CRDs are installed first",
			"x", "demo", "", err.Error())
	}
	for _, tc := range []struct {
		name        string
		err         error
		group, kind string
		versions    []string
	}{
		{
			name: "one version", err: helmWrap(nk("composition.krateo.io", "GithubProviderKog", "v0-6-0")),
			group: "composition.krateo.io", kind: "GithubProviderKog", versions: []string{"v0-6-0"},
		},
		{
			name: "one version in the core group", err: helmWrap(nk("", "ConfigMap", "v1")),
			group: "", kind: "ConfigMap", versions: []string{"v1"},
		},
		{
			name: "several versions", err: helmWrap(nk("composition.krateo.io", "FrontendCrds", "v1-8-15", "v1-8-14")),
			group: "composition.krateo.io", kind: "FrontendCrds", versions: []string{"v1-8-14", "v1-8-15"},
		},
		{
			name: "no version searched", err: helmWrap(nk("composition.krateo.io", "FrontendCrds")),
			group: "composition.krateo.io", kind: "FrontendCrds",
		},
		{
			// Two resources of the same kind both missing: the text names one kind, so it is known.
			name:  "the same kind twice",
			err:   errors.Join(helmWrap(nk("g.example.io", "Widget", "v1")), helmWrap(nk("g.example.io", "Widget", "v1"))),
			group: "g.example.io", kind: "Widget", versions: []string{"v1"},
		},
		{
			// A kind name %q has to escape is still read back exactly.
			name: "an escaped kind", err: helmWrap(nk("g.example.io", `We"ird`, "v1")),
			group: "g.example.io", kind: `We"ird`, versions: []string{"v1"},
		},
		// Every case below leaves the fields empty: the text does not name exactly one kind.
		{name: "two kinds", err: errors.Join(helmWrap(nk("g.example.io", "Widget", "v1")), helmWrap(nk("g.example.io", "Gadget", "v1")))},
		{name: "one kind in two groups", err: errors.Join(helmWrap(nk("a.example.io", "Widget", "v1")), helmWrap(nk("b.example.io", "Widget", "v1")))},
		{name: "a kind with no where", err: errors.New(`render step: no matches for kind "FooBar"`)},
		{name: "one parseable and one not", err: errors.Join(helmWrap(nk("g.example.io", "Widget", "v1")), errors.New(`no matches for kind "Gadget"`))},
		{name: "a version that is not a group/version", err: errors.New(`no matches for kind "Widget" in version "a/b/c"`)},
		{name: "an empty kind", err: errors.New(`no matches for kind "" in version "g.example.io/v1"`)},
		{name: "a versions list across groups", err: errors.New(`no matches for kind "Widget" in versions ["a.example.io/v1" "b.example.io/v1"]`)},
		{name: "only helm's hint", err: errors.New("could not build manifest: ensure CRDs are installed first")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !IsRESTMappingMiss(tc.err) {
				t.Fatal("not recognised as a mapping miss: the case tests nothing")
			}
			r := newMappingRetry("upgrade", tc.err)
			if r.Group != tc.group || r.Kind != tc.kind || !slices.Equal(r.Versions, tc.versions) {
				t.Errorf("read group=%q kind=%q versions=%q, want group=%q kind=%q versions=%q",
					r.Group, r.Kind, r.Versions, tc.group, tc.kind, tc.versions)
			}
		})
	}
}

// The typed error wins over the text: its versions keep their searched order, and a wrapping
// message that names another kind cannot override it.
func TestNewMappingRetry_TheTypedErrorWins(t *testing.T) {
	typed := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "g.example.io", Kind: "Widget"}, SearchedVersions: []string{"v2", "v1"}}
	err := fmt.Errorf(`while applying "no matches for kind \"Gadget\" in version \"h.example.io/v1\"": %w`, typed)
	r := newMappingRetry("install", err)
	if r.Group != "g.example.io" || r.Kind != "Widget" || !slices.Equal(r.Versions, []string{"v2", "v1"}) {
		t.Errorf("read %+v, want the typed error's g.example.io Widget [v2 v1]", r)
	}
}
