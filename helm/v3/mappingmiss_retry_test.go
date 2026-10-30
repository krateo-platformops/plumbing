package helm

import (
	"errors"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	memory "k8s.io/client-go/discovery/cached/memory"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/restmapper"
	clientgotesting "k8s.io/client-go/testing"
)

// TestIsRESTMappingMiss_Variants complements TestIsRESTMappingMiss in mappingmiss_test.go.
// That test already covers: nil, an unrelated error, helm's full stringified render error, the
// bare "resource mapping not found" substring, and a %w-wrapped typed *meta.NoKindMatchError.
// Here we pin down the REMAINING branches of IsRESTMappingMiss (read in client.go) so every
// variant it claims to match is exercised exactly once and false-positives stay excluded:
//   - meta.IsNoMatchError on a DIRECT (unwrapped) typed error,
//   - the "no matches for kind" substring on its own,
//   - the "ensure CRDs are installed first" substring on its own,
//   - two more unrelated errors (a wrapped one, and one whose text merely resembles but does
//     not contain any sentinel substring) that must NOT trip the matcher.
func TestIsRESTMappingMiss_Variants(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "direct typed NoKindMatchError (unwrapped, via meta.IsNoMatchError)",
			err: &meta.NoKindMatchError{
				GroupKind:        schema.GroupKind{Group: "composition.krateo.io", Kind: "SnowplowCrds"},
				SearchedVersions: []string{"v1-9-0"},
			},
			want: true,
		},
		{
			name: "no matches for kind substring standalone",
			err:  errors.New(`no matches for kind "Portal" in version "widgets.templates.krateo.io/v1beta1"`),
			want: true,
		},
		{
			name: "ensure CRDs are installed first substring standalone",
			err:  errors.New("could not build manifest: ensure CRDs are installed first"),
			want: true,
		},
		{
			name: "no matches for kind wrapped with %w",
			err:  fmt.Errorf("render step: %w", errors.New(`no matches for kind "FooBar"`)),
			want: true,
		},
		{
			name: "unrelated wrapped error",
			err:  fmt.Errorf("load chart: %w", errors.New("dial tcp: i/o timeout")),
			want: false,
		},
		{
			name: "resembling but non-matching text",
			err:  errors.New("mapping the values file failed to parse"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRESTMappingMiss(tc.err); got != tc.want {
				t.Fatalf("IsRESTMappingMiss(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// newMappingMissMapper builds the exact discovery/mapper stack the production client wires in
// CachedClients (client_getter.go NewCachedClients): a mutable fake discovery client wrapped by
// a MemCacheClient, wrapped by a DeferredDiscoveryRESTMapper. The returned *FakeDiscovery lets a
// test register a new APIResource at runtime to simulate a CRD that landed AFTER the mapper first
// populated its cache. It returns the concrete *DeferredDiscoveryRESTMapper because that is the
// SAME concrete type held by CachedClients.mapper on which client.Install calls Reset().
func newMappingMissMapper(seed ...*metav1.APIResourceList) (*restmapper.DeferredDiscoveryRESTMapper, *discoveryfake.FakeDiscovery) {
	fd := &discoveryfake.FakeDiscovery{Fake: &clientgotesting.Fake{}}
	fd.Resources = append(fd.Resources, seed...)
	memCache := memory.NewMemCacheClient(fd)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memCache)
	return mapper, fd
}

// TestMapperResetPicksUpNewlyRegisteredKind proves the reset-then-retry CONTRACT that
// client.Install depends on. In client.go, Install on an IsRESTMappingMiss error does exactly:
//
//	c.cachedClients.mapper.Reset()
//	// re-init the action config and run install() a second time
//
// The load-bearing claim is that mapper.Reset() causes the very next mapping attempt to observe a
// kind that was NOT resolvable before (the stale-discovery-cache bug: a composition CRD created
// moments earlier in the same bootstrap is invisible until the cache is dropped). We drive the
// SAME concrete *DeferredDiscoveryRESTMapper the client holds:
//
//	attempt 1 (pre-registration)  -> RESTMapping MUST miss with a NoKindMatchError
//	                                  (this is precisely what IsRESTMappingMiss classifies true)
//	register the CRD's resource   -> simulate the CRD add the informer would have raced
//	Reset()                       -> the single invalidation Install performs before its retry
//	attempt 2 (post-Reset)        -> RESTMapping MUST now succeed
//
// Without the Reset() between the two attempts the MemCacheClient keeps serving its first snapshot
// and attempt 2 would miss identically — so this asserts the reset is causally required, which is
// the whole point of the fix the cdc must wire.
//
// What an envtest/integration test would ADD (and this unit deliberately does not attempt, because
// client.Install requires a real chart render + apiserver and there is no unexported seam to
// intercept the two install() calls — see client.go, the mapper field is a concrete type, not an
// injectable interface): that Install invokes Reset EXACTLY ONCE and retries EXACTLY ONCE end to
// end. That behavioral count is covered by the integration suite in client_test.go (build tag
// integration). Here we prove the mechanism the retry relies on.
func TestMapperResetPicksUpNewlyRegisteredKind(t *testing.T) {
	gk := schema.GroupKind{Group: "composition.krateo.io", Kind: "SnowplowCrds"}
	const version = "v1-9-0"

	// Seed one unrelated, always-present group (core "v1"). The MemCacheClient treats a discovery
	// surface with ZERO groups as an error and forces the DeferredDiscoveryRESTMapper to reload on
	// every call (a noisy retry storm); a real cluster always exposes core/v1, so seeding it makes
	// attempt 1 a CLEAN NoKindMatchError for our target kind rather than a discovery failure — which
	// is exactly the stale-cache condition we want to model (discovery works, our new kind just is
	// not in it yet).
	baseGroup := &metav1.APIResourceList{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "pods", Namespaced: true, Kind: "Pod"},
		},
	}

	// Start without the target kind: it is unknown, exactly like a CRD that has not yet been
	// observed by the discovery cache.
	mapper, fd := newMappingMissMapper(baseGroup)

	// Attempt 1: must miss, and must miss in the way IsRESTMappingMiss classifies as a mapping miss.
	_, err := mapper.RESTMapping(gk, version)
	if err == nil {
		t.Fatalf("attempt 1: expected a mapping miss for %s/%s before registration, got nil", gk, version)
	}
	if !IsRESTMappingMiss(err) {
		t.Fatalf("attempt 1: error should be classified as a REST mapping miss, got %T: %v", err, err)
	}

	// Simulate the CRD landing: register its served resource so discovery now knows the kind
	// (keeping the pre-existing base group, as a real cluster would).
	fd.Resources = []*metav1.APIResourceList{
		baseGroup,
		{
			GroupVersion: gk.Group + "/" + version,
			APIResources: []metav1.APIResource{
				{
					Name:       "snowplowcrds",
					Namespaced: true,
					Kind:       gk.Kind,
				},
			},
		},
	}

	// Control check: WITHOUT a Reset the memcache still serves its first (empty) snapshot, so the
	// mapping is still a miss. This pins that Reset is causally required, not incidental.
	if _, err := mapper.RESTMapping(gk, version); err == nil {
		t.Fatalf("control: mapping unexpectedly succeeded before Reset — the memcache should still be stale")
	} else if !IsRESTMappingMiss(err) {
		t.Fatalf("control: pre-Reset error should still be a mapping miss, got %T: %v", err, err)
	}

	// This is the single line client.Install performs before retrying install().
	mapper.Reset()

	// Attempt 2 (the retry): the newly registered kind must now resolve.
	mapping, err := mapper.RESTMapping(gk, version)
	if err != nil {
		t.Fatalf("attempt 2 (post-Reset): expected %s/%s to resolve after Reset, got error: %v", gk, version, err)
	}
	if mapping == nil {
		t.Fatalf("attempt 2 (post-Reset): expected a non-nil RESTMapping")
	}
	if mapping.GroupVersionKind.Kind != gk.Kind {
		t.Fatalf("attempt 2 (post-Reset): resolved kind = %q, want %q", mapping.GroupVersionKind.Kind, gk.Kind)
	}
	if mapping.Resource.Resource != "snowplowcrds" {
		t.Fatalf("attempt 2 (post-Reset): resolved resource = %q, want %q", mapping.Resource.Resource, "snowplowcrds")
	}
}
