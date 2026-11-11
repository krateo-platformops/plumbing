package tracecontext

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// The root span's attribute keys: exactly these three, as unstructured-runtime
// sets them.
const (
	AttrObjectKind      = attribute.Key("k8s.object.kind")
	AttrObjectName      = attribute.Key("k8s.object.name")
	AttrObjectNamespace = attribute.Key("k8s.object.namespace")
)

// Object names the object a reconcile is about.
type Object struct {
	// Kind is the Kind alone, such as Repo, without its group.
	Kind string
	// Name is the object's name.
	Name string
	// Namespace is the object's namespace, "" for a cluster-scoped object.
	Namespace string
}

// inboundKey holds, with tracing off, the annotation's span context for a runtime's injectors. It is
// never the active span.
type inboundKey struct{}

// WithInbound returns ctx carrying sc as the inbound span context, for a runtime's injectors to fall
// back to when tracing is off. StartReconcile does this itself with a nil tracer; it is exported so
// a runtime can do the same on a path that does not start a span.
func WithInbound(ctx context.Context, sc trace.SpanContext) context.Context {
	if !sc.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, inboundKey{}, sc)
}

// Inbound is the span context StartReconcile stored with tracing OFF: the annotation's, when it was
// valid, else the invalid span context. Injectors fall back to it only when there is no active span;
// it never carries the krateo.io/traceparent-continued marker, and an outbound HTTP injector must
// not use it (provider-runtime's InjectHeader reads only the active span).
func Inbound(ctx context.Context) trace.SpanContext {
	sc, _ := ctx.Value(inboundKey{}).(trace.SpanContext)
	return sc
}

// StartReconcile opens the reconcile root span for obj, starting at start.
// Every reconcile is its own trace (docs/design-tracing.md, D8): the root is
// always a new root, never a child of d.Remote and never of a span already in
// ctx. When d.Remote is valid (modes Parent and Link) it carries one link to
// it; with NoParent it has none. Only the pass's first act step for a new value
// joins the upstream trace (StartActStep), which adds a second link, from the
// root to that act. The span is SpanKindServer, named reconcile, with exactly
// the attributes k8s.object.kind, k8s.object.name and k8s.object.namespace.
//
// With tracer nil it starts nothing and allocates nothing beyond the context value. It returns ctx
// carrying d.Remote, when that's valid, for the runtime's injectors only (Inbound), and the no-op
// span. tracer is the CALLER's: its instrumentation scope identifies the runtime.
func StartReconcile(ctx context.Context, tracer trace.Tracer, obj Object, d Decision, start time.Time) (context.Context, trace.Span) {
	if tracer == nil {
		return WithInbound(ctx, d.Remote), trace.SpanFromContext(context.Background())
	}
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithTimestamp(start),
		trace.WithAttributes(
			AttrObjectKind.String(obj.Kind),
			AttrObjectName.String(obj.Name),
			AttrObjectNamespace.String(obj.Namespace),
		),
		trace.WithNewRoot(),
	}
	if d.Mode != NoParent && d.Remote.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: d.Remote}))
	}
	return tracer.Start(ctx, SpanReconcile, opts...)
}

// StartActStep opens an act step, the create, update or delete span around
// the provider's call, for obj. With upstream valid, it is the first act for a
// new krateo.io/traceparent value (D8):
//
//   - its parent is upstream, so the act and the calls it makes join the
//     upstream trace;
//   - it carries one link back to the reconcile root in ctx;
//   - it carries the root's three k8s.object attributes, since it isn't the
//     root's child and its root is often not stored;
//   - the root gains a link to it (Span.AddLink), when the act is sampled. An
//     unsampled act, which a parent-based sampler makes under an unsampled
//     upstream, is never exported, so the root doesn't point at it.
//
// With upstream invalid it is an ordinary StartStep child of the root. With tracer
// nil it starts nothing.
func StartActStep(ctx context.Context, tracer trace.Tracer, obj Object, step string, upstream trace.SpanContext) (context.Context, trace.Span) {
	if tracer == nil || !upstream.IsValid() {
		return StartStep(ctx, tracer, step)
	}
	root := trace.SpanFromContext(ctx)
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			AttrObjectKind.String(obj.Kind),
			AttrObjectName.String(obj.Name),
			AttrObjectNamespace.String(obj.Namespace),
		),
	}
	if sc := root.SpanContext(); sc.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
	}
	actCtx, act := tracer.Start(trace.ContextWithRemoteSpanContext(ctx, upstream), step, opts...)
	if act.SpanContext().IsSampled() {
		root.AddLink(trace.Link{SpanContext: act.SpanContext()})
	}
	return actCtx, act
}

// StartStep opens the child span named step under the reconcile root in ctx,
// with tracer. It's SpanKindInternal and has no attributes of its own. With
// tracer nil it starts nothing and returns ctx and the no-op span.
func StartStep(ctx context.Context, tracer trace.Tracer, step string) (context.Context, trace.Span) {
	if tracer == nil {
		return ctx, trace.SpanFromContext(context.Background())
	}
	return tracer.Start(ctx, step, trace.WithSpanKind(trace.SpanKindInternal))
}
