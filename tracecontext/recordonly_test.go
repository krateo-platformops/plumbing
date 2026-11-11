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

// provider-runtime#74. The act links to its root, and the root to the act, each only when its
// TARGET is head-sampled: one rule for both directions. One row per sampled/dropped combination
// that leaves the act observable. Before v1.46.0 the act linked to an unsampled root as well (the
// third row), pointing an exported span at one that was never exported.
func TestStartActStepLinksOnlyToSampledTargets(t *testing.T) {
	for _, tc := range []struct {
		name               string
		sampler            sdktrace.Sampler // nil: sample everything
		rootSampled        bool
		actSampled         bool
		wantActToRoot      bool
		wantRootToActIfRec bool
	}{
		{"root sampled, act sampled", nil, true, true, true, true},
		{"root sampled, act recorded but not sampled", recordOnlyActs{}, true, false, true, false},
		{"root dropped, act sampled", dropRoots{}, false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tracetest.NewSpanRecorder()
			opts := []sdktrace.TracerProviderOption{sdktrace.WithSpanProcessor(rec)}
			if tc.sampler != nil {
				opts = append(opts, sdktrace.WithSampler(tc.sampler))
			}
			tp := sdktrace.NewTracerProvider(opts...)
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

			d := Decide(ann(AnnotationKeyTraceparent, tpA), "", time.Time{}, time.Now(), time.Minute)
			ctx, root := StartReconcile(context.Background(), tp.Tracer(testScope), obj, d, time.Now())
			_, act := StartActStep(ctx, tp.Tracer(testScope), obj, SpanUpdate, d.Remote)
			if root.SpanContext().IsSampled() != tc.rootSampled || act.SpanContext().IsSampled() != tc.actSampled {
				t.Fatalf("premise: root sampled %v, act sampled %v; want %v, %v",
					root.SpanContext().IsSampled(), act.SpanContext().IsSampled(), tc.rootSampled, tc.actSampled)
			}
			act.End()
			root.End()

			var gotAct, gotRoot sdktrace.ReadOnlySpan
			for _, s := range rec.Ended() {
				switch s.Name() {
				case SpanUpdate:
					gotAct = s
				case SpanReconcile:
					gotRoot = s
				}
			}
			if gotAct == nil {
				t.Fatal("control: the act must be recorded in every row")
			}
			actToRoot := false
			for _, l := range gotAct.Links() {
				if l.SpanContext.SpanID() == root.SpanContext().SpanID() {
					actToRoot = true
				}
			}
			if actToRoot != tc.wantActToRoot {
				t.Errorf("act -> root link = %v, want %v (act links %v)", actToRoot, tc.wantActToRoot, gotAct.Links())
			}
			if gotRoot != nil {
				rootToAct := false
				for _, l := range gotRoot.Links() {
					if l.SpanContext.SpanID() == act.SpanContext().SpanID() {
						rootToAct = true
					}
				}
				if rootToAct != tc.wantRootToActIfRec {
					t.Errorf("root -> act link = %v, want %v", rootToAct, tc.wantRootToActIfRec)
				}
			} else if tc.rootSampled {
				t.Error("control: a sampled root must be recorded")
			}
		})
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
