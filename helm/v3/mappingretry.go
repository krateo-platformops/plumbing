package helm

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/api/meta"
)

// MappingRetry describes one reset-and-retry that retryOnStaleMapping performed: an operation failed
// on a REST-mapping miss, the shared mapper was reset, and the operation ran again.
//
// WHY THIS EXISTS. The retry used to be silent, so after a failed render nobody could tell its two
// explanations apart: the retry did not fire (a defect here), or it fired, re-read discovery from
// the apiserver, and the version was still absent (the apiserver lagging a version just added to a
// CRD). Both look the same from outside: "no matches for kind", once or twice, then a heal. Measured
// on krateo-057 on 2026-11-11: core-provider wrote each CRD 6-13s before the miss, on the Upgrade
// path, which IS covered by the retry.
type MappingRetry struct {
	// Operation is the helm operation retried: "install", "upgrade", "uninstall" or "rollback".
	Operation string
	// Group, Kind and Version name the kind that failed to resolve on the FIRST attempt, when the
	// error carries a typed *meta.NoKindMatchError. Empty when it was recognised only by its text
	// (helm sometimes stringifies it), and then nothing more is known.
	Group, Kind string
	// Versions are the versions the mapper searched for Kind.
	Versions []string
	// Outcome is what the retry came to. See the MappingRetry* constants.
	Outcome string
}

// The closed set of MappingRetry.Outcome values.
const (
	// MappingRetryRecovered: the retry succeeded. The mapper was stale and the reset fixed it.
	MappingRetryRecovered = "recovered"
	// MappingRetryMissedAgain: the retry failed on a REST-mapping miss AGAIN, after the reset re-read
	// discovery. The apiserver does not (yet) serve the kind, which no client-side cache can fix.
	MappingRetryMissedAgain = "missed_again"
	// MappingRetryOtherError: the retry got past the mapping and failed on something else.
	MappingRetryOtherError = "other_error"
	// MappingRetryRebuildFailed: the action configuration could not be rebuilt for the retry, so it
	// never ran; the original mapping miss is returned.
	MappingRetryRebuildFailed = "rebuild_failed"
)

// EventMappingRetry is the span event recorded on the span active in the operation's context for
// every reset-and-retry, with the MappingRetry fields as attributes.
const EventMappingRetry = "helm.stale_mapping_retry"

// Attribute keys of EventMappingRetry.
const (
	AttrMappingRetryOperation = attribute.Key("helm.operation")
	AttrMappingRetryGroup     = attribute.Key("k8s.kind.group")
	AttrMappingRetryKind      = attribute.Key("k8s.kind.name")
	AttrMappingRetryVersions  = attribute.Key("k8s.kind.searched_versions")
	AttrMappingRetryOutcome   = attribute.Key("helm.mapping_retry.outcome")
)

// WithMappingRetryObserver calls fn once for every reset-and-retry the client performs, after the
// retry, with what happened. A caller logs it at its own level through its own logger; plumbing
// decides neither. fn runs on the operation's goroutine and must not block. A nil fn observes
// nothing. The span event (EventMappingRetry) is recorded either way.
func WithMappingRetryObserver(fn func(ctx context.Context, r MappingRetry)) ClientOption {
	return func(c *client) error {
		c.mappingRetryObserver = fn
		return nil
	}
}

// newMappingRetry describes the first attempt's miss.
func newMappingRetry(operation string, first error) MappingRetry {
	r := MappingRetry{Operation: operation}
	var nk *meta.NoKindMatchError
	if errors.As(first, &nk) {
		r.Group, r.Kind = nk.GroupKind.Group, nk.GroupKind.Kind
		r.Versions = append([]string(nil), nk.SearchedVersions...)
	}
	return r
}

// retryOutcome classifies what the retry came to.
func retryOutcome(retryErr error) string {
	switch {
	case retryErr == nil:
		return MappingRetryRecovered
	case IsRESTMappingMiss(retryErr):
		return MappingRetryMissedAgain
	default:
		return MappingRetryOtherError
	}
}

// reportMappingRetry records r as a span event on the span in ctx and hands it to the observer.
func (c *client) reportMappingRetry(ctx context.Context, r MappingRetry) {
	attrs := []attribute.KeyValue{AttrMappingRetryOperation.String(r.Operation), AttrMappingRetryOutcome.String(r.Outcome)}
	if r.Kind != "" {
		attrs = append(attrs, AttrMappingRetryGroup.String(r.Group), AttrMappingRetryKind.String(r.Kind),
			AttrMappingRetryVersions.StringSlice(r.Versions))
	}
	trace.SpanFromContext(ctx).AddEvent(EventMappingRetry, trace.WithAttributes(attrs...))
	if c.mappingRetryObserver != nil {
		c.mappingRetryObserver(ctx, r)
	}
}
