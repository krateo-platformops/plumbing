package helm

import (
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

// testRESTConfig returns the smallest *rest.Config that lets NewClient (and its construction-time
// probe.Init) succeed WITHOUT dialing the apiserver. The Host is a loopback address that is never
// contacted at construction: action.Configuration.Init and WithCRDInformer build their clients
// lazily (the CRD informer's List/Watch runs asynchronously in factory.Start and does not block or
// fail construction). This keeps the file to construction/wiring assertions; the live
// CRD-add -> mapper.Reset() path is covered by crdinformer_test.go
// (TestStartCRDInformer_Add_Update_Delete_InvokeInvalidate).
func testRESTConfig() *rest.Config {
	return &rest.Config{Host: "https://127.0.0.1:1"}
}

// TestWithCRDInformerSetsCachedClients asserts requirement (1): NewClient with WithCRDInformer wires
// the discovery/mapper cache (c.cachedClients) that the CRD informer resets, and does not error with a
// minimal rest.Config.
func TestWithCRDInformerSetsCachedClients(t *testing.T) {
	cfg := testRESTConfig()

	c, err := NewClient(cfg, WithCRDInformer(cfg, 1*time.Minute))
	if err != nil {
		t.Fatalf("NewClient with WithCRDInformer: unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if c.cachedClients == nil {
		t.Fatalf("expected cachedClients to be set by WithCRDInformer, got nil")
	}
	if c.cachedClients.mapper == nil {
		t.Fatalf("expected cachedClients.mapper to be non-nil (informer resets it on CRD change)")
	}
	if c.cachedClients.discoveryClient == nil {
		t.Fatalf("expected cachedClients.discoveryClient to be non-nil")
	}
	if c.crdInformerCancel == nil {
		t.Fatalf("expected crdInformerCancel to be set so Close() can stop the informer")
	}
}

// TestNewClientWithoutCRDInformerLeavesCachedClientsNil asserts requirement (2): this documents the
// current cdc gap — without WithCRDInformer there is no CRD informer and no shared cache to reset, so a
// stale discovery/RESTMapper wedges the umbrella render (crdExists misses a freshly-registered CRD).
func TestNewClientWithoutCRDInformerLeavesCachedClientsNil(t *testing.T) {
	cfg := testRESTConfig()

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if c.cachedClients != nil {
		t.Fatalf("expected cachedClients to be nil without WithCRDInformer (the cdc gap), got %#v", c.cachedClients)
	}
	if c.crdInformerCancel != nil {
		t.Fatalf("expected crdInformerCancel to be nil without WithCRDInformer, got non-nil")
	}
}

// TestWithCacheAndCRDInformerBothSucceed asserts requirement (3): the two options compose on one client
// — the disk cache and the shared CRD-informer cache are independent and both get wired.
func TestWithCacheAndCRDInformerBothSucceed(t *testing.T) {
	cfg := testRESTConfig()

	c, err := NewClient(cfg,
		testCache(t),
		WithCRDInformer(cfg, 1*time.Minute),
	)
	if err != nil {
		t.Fatalf("NewClient with WithCache + WithCRDInformer: unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if c.cache == nil {
		t.Fatalf("expected disk cache to be set by WithCache, got nil")
	}
	if c.cachedClients == nil {
		t.Fatalf("expected cachedClients to be set by WithCRDInformer, got nil")
	}
}

// TestWithCRDInformerBeforeCacheOrderIndependent asserts requirement (3), option order swapped: the
// functional options must not depend on application order.
func TestWithCRDInformerBeforeCacheOrderIndependent(t *testing.T) {
	cfg := testRESTConfig()

	c, err := NewClient(cfg,
		WithCRDInformer(cfg, 30*time.Second),
		testCache(t),
	)
	if err != nil {
		t.Fatalf("NewClient with WithCRDInformer + WithCache: unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if c.cache == nil {
		t.Fatalf("expected disk cache to be set by WithCache, got nil")
	}
	if c.cachedClients == nil {
		t.Fatalf("expected cachedClients to be set by WithCRDInformer, got nil")
	}
}

// TestCloseCancelsInformerAndIsIdempotent asserts requirement (4): Close() cancels the informer context
// (crdInformerCancel) without panicking and is safe to call more than once.
func TestCloseCancelsInformerAndIsIdempotent(t *testing.T) {
	cfg := testRESTConfig()

	c, err := NewClient(cfg, WithCRDInformer(cfg, 1*time.Minute))
	if err != nil {
		t.Fatalf("NewClient with WithCRDInformer: unexpected error: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: unexpected error: %v", err)
	}
	// Calling Close again must not panic and must not error (crdInformerCancel is idempotent, disk
	// cache is nil here).
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: unexpected error: %v", err)
	}
}

// TestCloseWithoutInformerNoPanic asserts requirement (4) for the no-informer path: Close() on a client
// built without WithCRDInformer (crdInformerCancel == nil) must not panic.
func TestCloseWithoutInformerNoPanic(t *testing.T) {
	cfg := testRESTConfig()

	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: unexpected error: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close without informer: unexpected error: %v", err)
	}
}
