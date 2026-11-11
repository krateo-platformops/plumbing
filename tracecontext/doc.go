// Package tracecontext is the krateo.io/traceparent rule both Krateo runtimes apply, so that a
// composition's reconcile trace (unstructured-runtime) and its provider's managed-resource trace
// (provider-runtime) relate to an upstream trace in the same way. It holds the pure parent-or-link
// rule (Decide), the reconcile root span (StartReconcile), the act step that joins the upstream
// trace (StartActStep), ordinary steps (StartStep), the inbound context for injectors (Inbound),
// and the three annotation keys.
//
// PORTED, NOT REWRITTEN. decide.go and start.go and their tests come verbatim from provider-runtime
// v1.14.1 pkg/tracing (docs/design-tracing.md §6.2, decisions D6 and D8), moved here on 2026-11-11
// so the rule has one implementation (Diego's decision; agreed with the provider-runtime session).
// The only changes are two seams, agreed with that session:
//
//   - the Start functions take the CALLER's trace.Tracer instead of a TracerProvider, because the
//     instrumentation scope must identify the runtime, not this package. A nil Tracer starts
//     nothing, exactly as a nil provider did, and the tracing-off path still allocates nothing;
//   - the inbound span context that StartReconcile stores with tracing off is exported as
//     WithInbound / Inbound, because the injectors that read it stay in each runtime.
//
// # The rule (D8)
//
// Every reconcile is its own trace: the root is always a new root, linked to the object's
// krateo.io/traceparent when it is valid. Only the pass's FIRST ACT for a new value joins the
// upstream trace, as the act step's parent; the root and the act then link to each other (the root
// to the act only when the act is sampled). A value already continued, or whose window has closed,
// is linked and never joined. See Decide for the ordered rules.
//
// # The contract each runtime implements itself
//
// The pure rule is here; its state is not, because it is tied to each runtime's reconciler. A
// runtime adopting this package must keep, per object:
//
//   - a memo keyed by namespaced name AND uid, holding the value it has marked or failed to mark and
//     when it first saw the current value (Decide's remembered and firstSeen), created only when
//     tracing is on, and dropped on NotFound (tracing on or off), on finalizer removal, on a uid or
//     value change, and when a read shows the marker;
//   - the marker krateo.io/traceparent-continued, written with value Decision.Marker when
//     Decision.WriteMarker says: BeforeFirstAction is just before the pass's first create, update
//     or delete (a pass that does not act writes nothing); Now is right after the root starts, past
//     the pause check. Write it with an optimistic-lock merge patch on a copy, copy back only the
//     resourceVersion and the marker, and requeue BEFORE acting on a conflict;
//   - join the upstream only once per value.
//
// provider-runtime's pkg/reconciler/traces.go is the reference implementation of that contract.
package tracecontext
