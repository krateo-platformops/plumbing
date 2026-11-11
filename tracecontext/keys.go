package tracecontext

import "time"

// The annotation keys of the cross-runtime trace contract. Both runtimes alias these: a composition
// and its provider's managed resource must read and write the same bytes.
const (
	// AnnotationKeyTraceparent carries a W3C traceparent onto an object another Krateo controller
	// reconciles.
	AnnotationKeyTraceparent = "krateo.io/traceparent"
	// AnnotationKeyTracestate carries the matching W3C tracestate.
	AnnotationKeyTracestate = "krateo.io/tracestate"
	// AnnotationKeyTraceparentContinued records the traceparent value this object's reconciler has
	// continued, so later passes link to it instead of joining it. It belongs to the object it is
	// written on: never copy it onto another object.
	AnnotationKeyTraceparentContinued = "krateo.io/traceparent-continued"
)

// DefaultContinueWindow is how long a new traceparent value keeps making Parent passes that don't
// act before Decide turns them into Link passes (provider-runtime's default; its docs/design-tracing.md).
const DefaultContinueWindow = 10 * time.Minute

// The span names this package starts. SpanReconcile is the root; act and step names are the
// caller's (provider-runtime uses create/update/delete; unstructured-runtime its own phases).
const (
	SpanReconcile = "reconcile"
)
