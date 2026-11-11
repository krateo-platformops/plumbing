package tracecontext

import "go.opentelemetry.io/otel/attribute"

// Span events and attribute keys both runtimes emit, so a query or a sampler rule reads the same
// bytes whichever runtime produced the span (agreed with the provider-runtime session, 2026-11-11).
// Runtime-specific events, such as provider-runtime's rate-limit governor events, stay in that
// runtime.
const (
	// EventReconcilePaused is recorded on the reconcile root when the pass returns at a pause
	// check without acting. The root is still emitted, because the controller IS alive; the event
	// is what tells a paused object from a freshly reconciled one (unstructured-runtime#78).
	EventReconcilePaused = "reconcile.paused"
	// EventMarkerFailed is recorded on the root when the krateo.io/traceparent-continued marker
	// write fails with anything but a conflict or NotFound (see the package doc's contract).
	EventMarkerFailed = "trace.marker_failed"
	// EventConflict is recorded when a write in the pass loses an optimistic-concurrency race,
	// with AttrStep naming where.
	EventConflict = "k8s.conflict"
)

// AttrStep names the step an EventConflict happened in. The KEY is shared; its VALUES are each
// runtime's own (provider-runtime's map its conflict sites). The string keeps provider-runtime's
// original "provider_runtime." prefix on purpose: renaming it would move provider-runtime's
// exported name and every query that already reads it.
const AttrStep = attribute.Key("provider_runtime.step")
