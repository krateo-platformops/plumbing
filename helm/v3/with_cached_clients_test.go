package helm

import (
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// krateo-core-projects#202: without WithCachedClients, a client whose cachedClients is nil falls to
// the uncached RESTClientGetter, whose ToRESTMapper builds a FULL API discovery. A caller that
// constructs a client per unit of work pays that per unit of work — measured as ~45% of 5.19 TB
// allocated over 16 hours in a controller that managed nothing.
//
// These assert the WIRING, because the failure mode is silent: a client that quietly builds its own
// mapper behaves identically to one that reuses a cached one, only slower, so nothing fails and
// nothing is logged. The only observable difference is which object the getter hands back.

func testConfig() *rest.Config {
	return &rest.Config{Host: "https://api.example.invalid:6443"}
}

func TestWithCachedClients_IsUsedByTheGetter(t *testing.T) {
	cc, err := NewCachedClients(testConfig())
	if err != nil {
		t.Fatalf("NewCachedClients: %v", err)
	}

	c, err := NewClient(testConfig(), WithCachedClients(&cc))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.cachedClients == nil {
		t.Fatal("WithCachedClients did not set cachedClients; the client will build its own " +
			"discovery mapper per operation, which is the cost this option exists to remove")
	}
	if c.cachedClients != &cc {
		t.Error("the client holds a different CachedClients than the one supplied; sharing one " +
			"instance across clients is the whole point, so a copy defeats it")
	}

	// The getter must actually consult it. ToRESTMapper on the cached path wraps the SUPPLIED
	// mapper in a shortcut expander; on the uncached path it constructs a new one from scratch.
	g := NewRESTClientGetterWithCachedClients(c.namespace, nil, c.restConfig, c.cachedClients)
	if g.cachedClients == nil || g.cachedClients.mapper != cc.mapper {
		t.Error("the REST client getter is not reading the supplied mapper, so ToRESTMapper would " +
			"fall through to a fresh full discovery")
	}
	if g.cachedClients.discoveryClient != cc.discoveryClient {
		t.Error("the getter is not reading the supplied discovery client")
	}
}

// The control arm: a client built WITHOUT the option must still work, and must not silently
// acquire a cache. That is the pre-existing behaviour and the thing this option is opt-in against —
// if it changed, every existing caller would start sharing state it never asked to share.
func TestWithoutCachedClients_StaysUncached(t *testing.T) {
	c, err := NewClient(testConfig())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.cachedClients != nil {
		t.Error("a client built without WithCachedClients acquired a cache anyway; this option " +
			"must be opt-in, because sharing a mapper is a decision the caller makes about its " +
			"own lifetime")
	}
}

// nil must be refused rather than stored. A nil CachedClients would make the client look cached
// while every getter check (`c.cachedClients != nil && c.cachedClients.mapper != nil`) fell
// through — the worst outcome, since the caller believes it opted in.
func TestWithCachedClients_RefusesNil(t *testing.T) {
	_, err := NewClient(testConfig(), WithCachedClients(nil))
	if err == nil {
		t.Fatal("WithCachedClients(nil) was accepted; the client would report as cached while " +
			"behaving uncached, which is worse than not offering the option")
	}
	if !strings.Contains(err.Error(), "must not be nil") {
		t.Errorf("the error does not say what was wrong: %v", err)
	}
}

// Two clients sharing one CachedClients must share the same mapper instance — that is the
// property that turns N discoveries into one. Asserted on identity rather than on behaviour,
// because behaviour here is only observable as elapsed time.
func TestWithCachedClients_SharedAcrossClients(t *testing.T) {
	cc, err := NewCachedClients(testConfig())
	if err != nil {
		t.Fatalf("NewCachedClients: %v", err)
	}
	a, err := NewClient(testConfig(), WithCachedClients(&cc))
	if err != nil {
		t.Fatalf("NewClient a: %v", err)
	}
	b, err := NewClient(testConfig(), WithCachedClients(&cc))
	if err != nil {
		t.Fatalf("NewClient b: %v", err)
	}
	if a.cachedClients.mapper != b.cachedClients.mapper {
		t.Error("two clients given the same CachedClients hold different mappers, so each would " +
			"still perform its own full discovery")
	}
}
