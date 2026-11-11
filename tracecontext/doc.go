// Package tracecontext is the krateo.io/traceparent rule both Krateo runtimes apply, so that a
// composition's reconcile trace (unstructured-runtime) and its provider's managed-resource trace
// (provider-runtime) relate to an upstream trace in the same way. It holds the pure parent-or-link
// rule (Decide), the reconcile root span (StartReconcile), the act step that joins the upstream
// trace (StartActStep), ordinary steps (StartStep), the inbound context for injectors (Inbound),
// and the three annotation keys. The design is provider-runtime's docs/design-tracing.md (§6.2,
// decisions D6 and D8).
//
// Two seams differ from where this code started:
//
//   - the Start functions take the CALLER's trace.Tracer, because the instrumentation scope must
//     identify the runtime, not this package. A nil Tracer starts nothing, and with it StartReconcile
//     allocates nothing for an object without krateo.io/traceparent. A non-nil Tracer, even a no-op
//     one, means tracing is on;
//   - the inbound span context StartReconcile stores with tracing off is exported as WithInbound /
//     Inbound, because the injectors that read it stay in each runtime.
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
//   - a memo keyed by namespaced name, holding the uid, and replaced when the uid or the canonical
//     value differs; created only when tracing is on and Decide returned a WriteMarker other than
//     Never; dropped on NotFound (tracing on or off), on finalizer removal, when the annotation goes
//     away or stops parsing, and when a read shows the marker. It supplies Decide's remembered and
//     firstSeen;
//   - the marker krateo.io/traceparent-continued, written with value Decision.Marker when
//     Decision.WriteMarker says: BeforeFirstAction is just before the pass's first create, update
//     or delete (a pass that does not act writes nothing); Now is right after the root starts, past
//     the pause check and any path that releases the object without acting, and before anything
//     acts. Write it with an optimistic-lock merge patch on a copy, copy back only the
//     resourceVersion and the marker, and requeue BEFORE acting on a conflict;
//   - the marker write's outcome: on success, or NotFound (the object is gone), remember the value
//     as marked; on a conflict, requeue before acting and leave the memo as it was, so the next
//     pass reads the object afresh; on any other error (RBAC, an admission webhook), remember the
//     value as failed, record the trace.marker_failed event on the root, log once per object, and
//     let the pass go on and act. Later passes pass a marked or failed value as Decide's
//     remembered, so they link; a seen-only value passes firstSeen and no remembered;
//   - write the marker at most once per pass;
//   - the act step: call StartActStep with d.Remote only when d.Mode is Parent, and only for the
//     pass's FIRST create, update or delete; every other act step, and every act in NoParent or
//     Link mode, is StartStep (or StartActStep with an invalid upstream). d.Remote is valid in
//     Link mode too, so passing it unconditionally re-joins the upstream on every acting pass.
//     The act step is the call that APPLIES changes, and only the code making it knows whether
//     it does. provider-runtime's act steps are Create/Update/Delete, its Observe being a pure
//     read. unstructured-runtime opens one around Create/Update/Delete too, and a handler whose
//     Observe applies changes declares its act there with controller.StartAct, right before
//     the mutating call: the composition-dynamic-controller's Observe applies the release's
//     children (plumbing helm Reconcile) only when the release exists, so treating every
//     Observe as the act would join a new object's upstream at an Observe that applied nothing
//     and leave the install that follows linked, not joined. The marker is written before the
//     act, so a conflict delays that act by one requeue.
//
// provider-runtime's pkg/reconciler/traces.go is the reference implementation of that contract.
//
// # Not shared
//
// Left to each runtime, deliberately: error and status handling on spans (provider-runtime cuts an
// error description at 4096 bytes, records an exception event, never sets Ok, and records a panic
// as "panic"); the act and step names and the instrumentation scope, which belong to the caller;
// what Object carries (Kind without its group, Namespace "" for a cluster-scoped object); and which
// context an injector stamps a child from, which decides whether the downstream work chains to this
// pass.
package tracecontext
