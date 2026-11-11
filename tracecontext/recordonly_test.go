package tracecontext

import (
	"context"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// recordOnlyActs samples every span except the act step, which it records without sampling: the
// shape a RecordOnly sampler produces, where IsRecording is true and IsSampled is false.
type recordOnlyActs struct{}

func (recordOnlyActs) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if p.Name == SpanUpdate {
		return sdktrace.SamplingResult{Decision: sdktrace.RecordOnly}
	}
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}
func (recordOnlyActs) Description() string { return "recordOnlyActs" }

// provider-runtime#72. The root links to the act only when the act is SAMPLED: an act that is
// recorded but not sampled is never exported, so a link to it would point at nothing. The guard is
// IsSampled, deliberately not IsRecording, and this is the case that tells them apart.
func TestStartActStepRecordOnlyActGetsNoRootLink(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithSampler(recordOnlyActs{}))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	d := Decide(ann(AnnotationKeyTraceparent, tpA), "", time.Time{}, time.Now(), time.Minute)
	ctx, root := StartReconcile(context.Background(), tp.Tracer(testScope), obj, d, time.Now())
	_, act := StartActStep(ctx, tp.Tracer(testScope), obj, SpanUpdate, d.Remote)
	// PREMISE: the act is recorded but not sampled, or this test cannot tell the guards apart.
	if !act.IsRecording() || act.SpanContext().IsSampled() {
		t.Fatalf("premise: act recording=%v sampled=%v, want true/false", act.IsRecording(), act.SpanContext().IsSampled())
	}
	act.End()
	root.End()

	var rootSpan sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == SpanReconcile {
			rootSpan = s
		}
	}
	if rootSpan == nil {
		t.Fatal("the root was not recorded")
	}
	for _, l := range rootSpan.Links() {
		if l.SpanContext.SpanID() == act.SpanContext().SpanID() {
			t.Errorf("the root links to a recorded-but-unsampled act, which is never exported")
		}
	}
	// Control: the root's own link to the upstream annotation is still there.
	if n := len(rootSpan.Links()); n != 1 || rootSpan.Links()[0].SpanContext.SpanID() != d.Remote.SpanID() {
		t.Errorf("root links = %d, want exactly the upstream link", n)
	}
}

func TestInbound(t *testing.T) {
	ctx := context.Background()
	if Inbound(ctx).IsValid() {
		t.Error("a bare context has an inbound span context")
	}
	if got := WithInbound(ctx, trace.SpanContext{}); got != ctx {
		t.Error("WithInbound with an invalid span context must return ctx unchanged (no allocation)")
	}
	d := Decide(ann(AnnotationKeyTraceparent, tpA), "", time.Time{}, time.Now(), time.Minute)
	if got := Inbound(WithInbound(ctx, d.Remote)); got.SpanID() != d.Remote.SpanID() {
		t.Error("Inbound does not return what WithInbound stored")
	}
	// It is never the ACTIVE span: injectors read the active span first and fall back to Inbound.
	if trace.SpanContextFromContext(WithInbound(ctx, d.Remote)).IsValid() {
		t.Error("the inbound span context became the active span")
	}
}

// dropRoots drops the reconcile root and samples everything else.
type dropRoots struct{}

func (dropRoots) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if p.Name == SpanReconcile {
		return sdktrace.SamplingResult{Decision: sdktrace.Drop}
	}
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}
func (dropRoots) Description() string { return "dropRoots" }

// PINS v1.14.1 AS IT IS, not as it should be: the act links to its root even when the root is
// unsampled, so an exported act can point at a root that was never exported. The provider-runtime
// session flagged the asymmetry (the root links to the act only when the act is sampled) and is
// deciding it there. This test makes a change to it deliberate: update it with that decision.
func TestStartActStepLinksToAnUnsampledRootAsInV1141(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithSampler(dropRoots{}))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	d := Decide(ann(AnnotationKeyTraceparent, tpA), "", time.Time{}, time.Now(), time.Minute)
	ctx, root := StartReconcile(context.Background(), tp.Tracer(testScope), obj, d, time.Now())
	if root.SpanContext().IsSampled() {
		t.Fatal("premise: the root must be unsampled")
	}
	_, act := StartActStep(ctx, tp.Tracer(testScope), obj, SpanUpdate, d.Remote)
	act.End()
	root.End()
	var got sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == SpanUpdate {
			got = s
		}
	}
	if got == nil {
		t.Fatal("the act was not recorded")
	}
	if l := got.Links(); len(l) != 1 || l[0].SpanContext.SpanID() != root.SpanContext().SpanID() {
		t.Errorf("act links = %v; v1.14.1 links the act to its root even when the root is unsampled", l)
	}
}

// StartActStep marks the upstream as the REMOTE parent itself, whatever the caller hands it. Every
// upstream Decide produces is already remote (the W3C propagator's Extract sets the flag), so for
// those inputs ContextWithSpanContext and ContextWithRemoteSpanContext are indistinguishable. This
// arm passes a locally built, non-remote span context, which is the only input that tells them
// apart.
func TestStartActStepMarksTheUpstreamRemote(t *testing.T) {
	tp, rec := recorder(t)
	local := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	if local.IsRemote() {
		t.Fatal("premise: the constructed upstream must not already be remote")
	}
	ctx, root := StartReconcile(context.Background(), tp.Tracer(testScope), obj, Decision{}, time.Now())
	_, act := StartActStep(ctx, tp.Tracer(testScope), obj, SpanUpdate, local)
	act.End()
	root.End()
	for _, s := range rec.Ended() {
		if s.Name() == SpanUpdate && !s.Parent().IsRemote() {
			t.Error("StartActStep must make the upstream a REMOTE parent, even when the caller's span context is not marked remote")
		}
	}
}
