// Package fieldpath parses a field path into typed segments.
//
// It exists because the same dialect was implemented three times, in three modules, with three different
// accepted grammars and no test comparing them:
//
//	oasgen-provider       dot notation only -- and its doc comment claimed bracket support it refused
//	rest-dynamic-controller  dots, quoted keys, array indices and [?key=value] predicates
//	unstructured-runtime  strings.Split(path, ".") -- silently mis-splits a quoted key containing a dot
//
// This package is the superset (rest-dynamic-controller's grammar), with one correction: a segment's KIND
// is decided by HOW IT WAS WRITTEN, never by its content.
//
// # Why kind-by-syntax rather than kind-by-content
//
// The previous implementation returned []string and the walker re-derived intent from each segment's text.
// That made two pairs of distinct paths indistinguishable:
//
//	a[0]  and a['0']    -> both "0"        a list index and a map key
//	a[?k=v] and a['?k=v'] -> both "?k=v"   a selector and a literal key
//
// Reading, the data disambiguated the first pair -- a value is a map or a list, never both -- so it was
// never a live defect for that walker. The second pair is worse: quoting is what a user reaches for to
// escape a special meaning, and an escape that does not escape is worse than no escape at all. And
// neither pair can be recovered by a WRITER (create a map or a list?), a VALIDATOR, or a diagnostic:
// `a['0']` against a list reports "array requires a non-negative integer index", which does not mention
// that the author asked for a key.
//
// Deciding kind at parse time, from syntax, removes the content sniffing entirely.
//
// # Limits of the grammar
//
// There is no escape sequence. Inside a bracket group every quote character TOGGLES quoting until its
// match, which has these consequences, each pinned by a test:
//
//   - a quoted key may contain the other quote character freely, and its own quote character only in
//     balanced pairs: ["it's "x""] is the key it's "x". A key with a ' and an ODD number of " (or the
//     reverse) cannot be written; String renders it, and the rendering is refused by Parse.
//   - a predicate value may contain quote characters only in balanced pairs, and they are KEPT in the
//     value: a[?name=say "hi"] selects name == say "hi". An unbalanced one, as in a[?name=O'Brien], leaves
//     the bracket unterminated and the path is refused. A ']' cannot appear in a predicate value.
//
// Refused paths are refused with an error, never mis-parsed into something else.
//
// # Whitespace
//
// A bare name may not contain whitespace: a. b and "b " are refused, not read as the keys " b" and
// "b ". A key with whitespace is written quoted, a['my key']; a predicate value may contain it,
// a[?name=John Smith]. rest-dynamic-controller's parser refused every space anywhere, which also
// refused those two legitimate forms.
//
// # The parser/walker boundary
//
// This package decides only what a path MEANS, never whether it applies to a document. A predicate
// against a non-list, a predicate matching no element, and a predicate matching more than one are all
// WALKER errors: the path was well-formed and the document did not satisfy it. Keeping that boundary is
// what stops content sniffing being reintroduced -- a parser that knew about documents would need it.
package fieldpath

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Kind is how a segment was written, which is what decides its meaning.
type Kind int

const (
	// KindKey is a map key: a bare name (a.b), or a quoted bracket group (a['b.c'], a["b.c"]).
	KindKey Kind = iota
	// KindIndex is a list index: an UNQUOTED bracket group of canonical digits (a[0]).
	KindIndex
	// KindPredicate selects the single list element whose Key field equals Value (a[?name=admin]).
	KindPredicate
)

func (k Kind) String() string {
	switch k {
	case KindKey:
		return "key"
	case KindIndex:
		return "index"
	case KindPredicate:
		return "predicate"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Segment is one step of a path.
type Segment struct {
	Kind  Kind
	Key   string // a map key, or a predicate's field name
	Index int    // KindIndex only
	Value string // KindPredicate only; may be empty, which is accepted
}

// String renders the segment in a form Parse accepts, so a parsed path round-trips. A key that needs
// quoting is quoted with ' unless it contains one, then with ". See "Limits" for the keys no quoting
// can write.
func (s Segment) String() string {
	switch s.Kind {
	case KindIndex:
		return "[" + strconv.Itoa(s.Index) + "]"
	case KindPredicate:
		return "[?" + s.Key + "=" + s.Value + "]"
	}
	if strings.ContainsAny(s.Key, ".[]'\"?") || s.Key == "" || hasSpace(s.Key) {
		q := "'"
		if strings.Contains(s.Key, "'") {
			q = `"`
		}
		return "[" + q + s.Key + q + "]"
	}
	return s.Key
}

// Keys returns the flat form every pre-existing caller used: each segment's text, with an index rendered
// as its decimal digits and a predicate as "?key=value". It is lossy by design -- a[0] and a['0'] produce
// the same slice -- and exists so callers that genuinely only need the text are unchanged.
func Keys(segs []Segment) []string {
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch s.Kind {
		case KindIndex:
			out = append(out, strconv.Itoa(s.Index))
		case KindPredicate:
			out = append(out, "?"+s.Key+"="+s.Value)
		default:
			out = append(out, s.Key)
		}
	}
	return out
}

// Parse splits path into typed segments.
//
// Leading dots are tolerated and ignored, for compatibility: paths of the form ".id" exist in the wild
// because two documented dialects disagreed, and rejecting them now would break definitions that work
// today. Do not tighten this without a migration.
func Parse(path string) ([]Segment, error) {
	if path == "" {
		return nil, fmt.Errorf("malformed path: empty")
	}
	rest := strings.TrimLeft(path, ".")
	if rest == "" {
		return nil, fmt.Errorf("malformed path %q: nothing but separators", path)
	}
	if strings.HasSuffix(rest, ".") {
		return nil, fmt.Errorf("malformed path %q: trailing separator", path)
	}

	var segs []Segment
	for _, token := range splitTopLevel(rest) {
		if token == "" {
			return nil, fmt.Errorf("malformed path %q: empty segment", path)
		}
		got, err := parseToken(token)
		if err != nil {
			return nil, fmt.Errorf("malformed path %q: %w", path, err)
		}
		segs = append(segs, got...)
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("malformed path %q: no segments", path)
	}
	return segs, nil
}

// splitTopLevel splits on dots that are outside brackets and outside quotes, so a quoted key may contain
// a dot and a predicate value may contain one.
func splitTopLevel(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == '[':
			depth++
			cur.WriteRune(r)
		case r == ']':
			depth--
			cur.WriteRune(r)
		case r == '.' && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	return out
}

// parseToken parses one dot-separated token into one or more segments. A token is a bare name, or a name
// followed by one or more bracket groups, or bracket groups alone.
func parseToken(token string) ([]Segment, error) {
	open := strings.IndexByte(token, '[')
	if open < 0 {
		if strings.ContainsAny(token, "]'\"") {
			return nil, fmt.Errorf("segment %q: stray bracket or quote", token)
		}
		if hasSpace(token) {
			return nil, fmt.Errorf("segment %q: unquoted whitespace; quote the key, as in ['my key']", token)
		}
		return []Segment{{Kind: KindKey, Key: token}}, nil
	}

	var segs []Segment
	if open > 0 {
		name := token[:open]
		if strings.ContainsAny(name, "]'\"") {
			return nil, fmt.Errorf("segment %q: stray bracket or quote before '['", token)
		}
		if hasSpace(name) {
			return nil, fmt.Errorf("segment %q: unquoted whitespace; quote the key, as in ['my key']", token)
		}
		segs = append(segs, Segment{Kind: KindKey, Key: name})
	}

	for rest := token[open:]; rest != ""; {
		if rest[0] != '[' {
			return nil, fmt.Errorf("segment %q: expected '[' after a bracket group", token)
		}
		end := closingBracket(rest)
		if end < 0 {
			return nil, fmt.Errorf("segment %q: unterminated bracket", token)
		}
		inner := rest[1:end]
		got, err := parseBracket(inner)
		if err != nil {
			return nil, fmt.Errorf("segment %q: %w", token, err)
		}
		segs = append(segs, got)
		rest = rest[end+1:]
	}
	return segs, nil
}

// closingBracket returns the index of the ']' that closes the group starting at rest[0], skipping any ']'
// inside a quoted key. A naive IndexByte finds the first one, which breaks a['[0]'] -- a quoted key must
// be able to contain any character, since quoting it is the only way to say so.
func closingBracket(rest string) int {
	var quote byte
	for i := 1; i < len(rest); i++ {
		c := rest[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ']':
			return i
		}
	}
	return -1
}

// parseBracket parses the inside of one bracket group. The three forms are distinguished by SYNTAX:
// quoted means a key, leading '?' means a predicate, bare digits mean an index. Anything else is refused
// rather than guessed at.
func parseBracket(inner string) (Segment, error) {
	if len(inner) >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[len(inner)-1] == inner[0] {
		return Segment{Kind: KindKey, Key: inner[1 : len(inner)-1]}, nil
	}
	if inner != "" && (inner[0] == '\'' || inner[0] == '"') {
		return Segment{}, fmt.Errorf("unterminated quote in bracket")
	}
	if strings.HasPrefix(inner, "?") {
		body := inner[1:]
		eq := strings.IndexByte(body, '=')
		if eq <= 0 {
			// An empty KEY is refused; an empty VALUE is accepted, matching the behaviour this grammar
			// replaces. a[?=v] is rejected, a[?k=] is not.
			return Segment{}, fmt.Errorf("predicate must be [?key=value] with a non-empty key")
		}
		return Segment{Kind: KindPredicate, Key: body[:eq], Value: body[eq+1:]}, nil
	}
	if inner == "" {
		return Segment{}, fmt.Errorf("empty bracket")
	}
	if !isCanonicalDigits(inner) {
		return Segment{}, fmt.Errorf("bracket must contain a quoted string, a [?key=value] predicate, or canonical digits")
	}
	n, err := strconv.Atoi(inner)
	if err != nil {
		return Segment{}, fmt.Errorf("bracket index %q: %w", inner, err)
	}
	return Segment{Kind: KindIndex, Index: n}, nil
}

// isCanonicalDigits accepts "0" and "123" but not "", "01", "-1" or "1x". Leading zeros are refused
// because they are the shape of a quoted key written without quotes, and guessing which was meant is the
// content sniffing this package exists to remove.
func isCanonicalDigits(s string) bool {
	if s == "" {
		return false
	}
	if len(s) > 1 && s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// hasSpace reports whether s contains any whitespace. A BARE name may not: outside brackets whitespace
// is almost always a mistake (a. b, YAML trailing space) and used to become part of the key silently,
// so " b" was looked up instead of "b". Inside brackets it is delimited, so a quoted key or a predicate
// value may contain it.
func hasSpace(s string) bool { return strings.IndexFunc(s, unicode.IsSpace) >= 0 }
