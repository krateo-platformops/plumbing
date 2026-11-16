package helm

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// Group and Kind name the kind that failed to resolve on the FIRST attempt. They come from a
	// typed *meta.NoKindMatchError when the error carries one. helm sometimes stringifies it, and
	// then they are read from the text apimachinery wrote (#191). They are empty when neither
	// names exactly one kind: an unparseable text, or a text naming more than one kind, is not
	// guessed at.
	Group, Kind string
	// Versions are the versions the mapper searched for Kind, without the group. Read from the
	// text they are in apimachinery's order, which is sorted.
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
		return r
	}
	if first != nil {
		if m, ok := noKindMatchFromText(first.Error()); ok {
			r.Group, r.Kind, r.Versions = m.group, m.kind, m.versions
		}
	}
	return r
}

// quoted is one Go-quoted string, as fmt's %q writes it.
const quoted = `"(?:[^"\\]|\\.)*"`

// noKindMatchText matches the three forms meta.NoKindMatchError.Error() writes (apimachinery
// pkg/api/meta/errors.go), by how many versions were searched:
//
//	no matches for kind "K" in group "G"                    none
//	no matches for kind "K" in version "G/V"                one ("V" alone for the core group)
//	no matches for kind "K" in versions ["G/V1" "G/V2"]     several (%q of a []string)
var (
	noKindMatchText = regexp.MustCompile(`no matches for kind (` + quoted + `) in (group|version|versions) (` +
		quoted + `|\[(?:` + quoted + `(?: ` + quoted + `)*)?\])`)
	quotedText = regexp.MustCompile(quoted)
)

const noKindMatchPrefix = "no matches for kind"

type kindMatch struct {
	group, kind string
	versions    []string
}

func (a kindMatch) equal(b kindMatch) bool {
	return a.group == b.group && a.kind == b.kind && slices.Equal(a.versions, b.versions)
}

// noKindMatchFromText reads the kind a stringified NoKindMatchError names. It reports ok only when
// every "no matches for kind" in s parses and all of them name the same group, kind and versions:
// a text it cannot fully read, or one naming two kinds, would make any single answer a guess.
func noKindMatchFromText(s string) (kindMatch, bool) {
	all := noKindMatchText.FindAllStringSubmatch(s, -1)
	if len(all) == 0 || len(all) != strings.Count(s, noKindMatchPrefix) {
		return kindMatch{}, false
	}
	var first kindMatch
	for i, sm := range all {
		m, ok := parseNoKindMatch(sm[1], sm[2], sm[3])
		if !ok || (i > 0 && !m.equal(first)) {
			return kindMatch{}, false
		}
		if i == 0 {
			first = m
		}
	}
	return first, true
}

func parseNoKindMatch(qKind, form, rest string) (kindMatch, bool) {
	kind, err := strconv.Unquote(qKind)
	if err != nil || kind == "" {
		return kindMatch{}, false
	}
	m := kindMatch{kind: kind}
	switch form {
	case "group":
		if m.group, err = strconv.Unquote(rest); err != nil {
			return kindMatch{}, false
		}
		return m, true
	case "version":
		gv, ok := parseGroupVersion(rest)
		if !ok {
			return kindMatch{}, false
		}
		m.group, m.versions = gv.Group, []string{gv.Version}
		return m, true
	default: // "versions"
		items := quotedText.FindAllString(rest, -1)
		if len(items) < 2 {
			return kindMatch{}, false
		}
		for i, q := range items {
			gv, ok := parseGroupVersion(q)
			if !ok || (i > 0 && gv.Group != m.group) {
				return kindMatch{}, false
			}
			m.group = gv.Group
			m.versions = append(m.versions, gv.Version)
		}
		return m, true
	}
}

func parseGroupVersion(q string) (schema.GroupVersion, bool) {
	s, err := strconv.Unquote(q)
	if err != nil || s == "" {
		return schema.GroupVersion{}, false
	}
	gv, err := schema.ParseGroupVersion(s)
	if err != nil || gv.Version == "" {
		return schema.GroupVersion{}, false
	}
	return gv, true
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
