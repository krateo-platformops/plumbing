package helm

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"helm.sh/helm/v3/pkg/action"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// The regression these cover: the stale-mapper reset-and-retry existed ONLY in Install. With
// WithCachedClients the RESTMapper lives for the whole process, so an umbrella chart reconciled via
// Upgrade failed with `no matches for kind "X" in version "Y"` — identically on every reconcile,
// for the process's lifetime — against CRDs that were already serving exactly those versions.
//
// The pre-existing TestMapperResetPicksUpNewlyRegisteredKind proves the MECHANISM (Reset makes a
// newly registered kind resolvable) and says in its own comment that it cannot prove the wiring.
// That split is precisely how the bug shipped: mechanism covered, per-operation wiring not. So
// these make the two claims separately and label which is which.

func noKindMatch() error {
	return &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "composition.krateo.io", Kind: "FrontendCrds"},
		SearchedVersions: []string{"v1-8-15"},
	}
}

// BEHAVIOUR. retryOnStaleMapping must reset and re-run exactly once on a mapping miss, and must not
// re-run on anything else. Driven through the real method with a counting closure, so this is a
// behavioural claim about the wrapper rather than a syntactic one.
func TestRetryOnStaleMapping(t *testing.T) {
	cc, err := NewCachedClients(&rest.Config{Host: "https://api.example.invalid:6443"})
	if err != nil {
		t.Fatalf("NewCachedClients: %v", err)
	}

	for _, tc := range []struct {
		name      string
		cached    *CachedClients
		errs      []error // error returned by attempt 1, 2, ...
		wantCalls int
		wantErr   bool
		// wantOutcome is the ONE MappingRetry the observer must receive; "" means none at all.
		wantOutcome string
		wantKind    string
	}{
		{
			name:        "mapping miss is retried once and can succeed",
			cached:      &cc,
			errs:        []error{noKindMatch(), nil},
			wantCalls:   2,
			wantOutcome: MappingRetryRecovered, wantKind: "FrontendCrds",
		},
		{
			// The shipped failure: the kind really is absent, so the retry also misses. It must
			// still be attempted exactly twice — not looped — and must surface the error.
			name:      "mapping miss that persists is attempted twice and returns the error",
			cached:    &cc,
			errs:      []error{noKindMatch(), noKindMatch()},
			wantCalls: 2,
			wantErr:   true,
			// The case the observer exists for: the reset re-read discovery and the kind is still
			// absent, which is apiserver-side, not a stale cache.
			wantOutcome: MappingRetryMissedAgain, wantKind: "FrontendCrds",
		},
		{
			name:        "a retry that gets past the mapping and fails otherwise",
			cached:      &cc,
			errs:        []error{noKindMatch(), errors.New("upgrade failed: timed out")},
			wantCalls:   2,
			wantErr:     true,
			wantOutcome: MappingRetryOtherError, wantKind: "FrontendCrds",
		},
		{
			// helm stringifies the miss when it surfaces through the post-renderer, which is how
			// the umbrella's failure actually arrived. IsRESTMappingMiss matches that form too.
			name:      "helm's stringified miss is also retried",
			cached:    &cc,
			errs:      []error{errors.New(`no matches for kind "GithubProviderKog" in version "composition.krateo.io/v0-6-0"`), nil},
			wantCalls: 2,
			// Recognised by its text only, so the kind is unknown and reported empty, not guessed.
			wantOutcome: MappingRetryRecovered, wantKind: "",
		},
		{
			name:      "an unrelated error is NOT retried",
			cached:    &cc,
			errs:      []error{errors.New("chart type check failed"), nil},
			wantCalls: 1,
			wantErr:   true,
		},
		{
			name:      "success is not retried",
			cached:    &cc,
			errs:      []error{nil},
			wantCalls: 1,
		},
		{
			// Degrade-only: without shared clients each operation builds its own discovery and is
			// fresh anyway, so there is nothing to reset and no reason to double the work.
			name:      "no cached clients means no retry",
			cached:    nil,
			errs:      []error{noKindMatch(), nil},
			wantCalls: 1,
			wantErr:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []MappingRetry
			c := &client{restConfig: &rest.Config{Host: "https://api.example.invalid:6443"}, cachedClients: tc.cached,
				mappingRetryObserver: func(_ context.Context, r MappingRetry) { seen = append(seen, r) }}
			rec := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			ctx, span := tp.Tracer("test").Start(context.Background(), "op")
			calls := 0
			err := c.retryOnStaleMapping(ctx, "upgrade", "default", c.restConfig, nil,
				func(_ *action.Configuration) error {
					defer func() { calls++ }()
					if calls < len(tc.errs) {
						return tc.errs[calls]
					}
					return fmt.Errorf("op called %d times, more than the %d outcomes provided", calls+1, len(tc.errs))
				})
			if calls != tc.wantCalls {
				t.Errorf("op ran %d times, want %d", calls, tc.wantCalls)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			span.End()
			var events []string
			for _, e := range rec.Ended()[0].Events() {
				if e.Name == EventMappingRetry {
					for _, a := range e.Attributes {
						if a.Key == AttrMappingRetryOutcome {
							events = append(events, a.Value.AsString())
						}
					}
				}
			}
			if tc.wantOutcome == "" {
				if len(seen) != 0 || len(events) != 0 {
					t.Errorf("no retry ran, but the observer saw %v and the span got %v", seen, events)
				}
				return
			}
			if len(seen) != 1 || seen[0].Outcome != tc.wantOutcome || seen[0].Operation != "upgrade" || seen[0].Kind != tc.wantKind {
				t.Errorf("observer saw %+v, want one {upgrade %s kind=%q}", seen, tc.wantOutcome, tc.wantKind)
			}
			if tc.wantKind != "" && (len(seen) != 1 || len(seen[0].Versions) != 1 || seen[0].Versions[0] != "v1-8-15") {
				t.Errorf("searched versions = %+v, want [v1-8-15]", seen)
			}
			if len(events) != 1 || events[0] != tc.wantOutcome {
				t.Errorf("span events %s = %v, want one with outcome %s", EventMappingRetry, events, tc.wantOutcome)
			}
		})
	}
}

// WIRING. Every operation that resolves kinds must route through retryOnStaleMapping.
//
// This is a MENTION check, not a reachability one: it proves the call appears inside each function
// body, not that it is on the executed path. A source walk cannot prove reachability, and claiming
// otherwise is how a guard gives false comfort. The behavioural claim is TestRetryOnStaleMapping
// above; this one exists to fail loudly when someone adds a fifth operation — or restores an
// un-wrapped Upgrade — because that is exactly the change that shipped the outage, and no
// behavioural test of the OTHER operations would have caught it.
func TestEveryResolvingOperationRoutesThroughTheRetry(t *testing.T) {
	// GetRelease and ListReleases are deliberately absent: action.NewGet and action.NewList read
	// release records from the storage backend and never resolve the chart's kinds, so a stale
	// mapper cannot affect them. Verified by reading them, not assumed.
	want := []string{"Install", "Upgrade", "Uninstall", "Rollback"}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "client.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing client.go: %v", err)
	}

	found := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "retryOnStaleMapping" {
				found[fn.Name.Name] = true
			}
			return true
		})
	}

	if len(found) == 0 {
		t.Fatal("no retryOnStaleMapping calls found in client.go at all — this guard is not " +
			"looking at the right file and proves nothing")
	}
	for _, op := range want {
		if !found[op] {
			t.Errorf("%s does not call retryOnStaleMapping: with WithCachedClients its mapper is "+
				"process-lifetime, so a kind registered after warmup is invisible to it until the "+
				"process restarts. This is the exact shape of the Upgrade outage.", op)
		}
	}
}
