package fieldpath

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func k(s string) Segment    { return Segment{Kind: KindKey, Key: s} }
func i(n int) Segment       { return Segment{Kind: KindIndex, Index: n} }
func p(a, b string) Segment { return Segment{Kind: KindPredicate, Key: a, Value: b} }

// TestTheGrammar is the single pinned table. Every row is either a behaviour this package INHERITS from
// rest-dynamic-controller's parser (so adopting this one is a refactor there, not a change) or a
// correction that is called out in the `why` column.
func TestTheGrammar(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []Segment
		err  string // non-empty means Parse must fail and the message must contain this
		why  string
	}{
		// --- plain dot notation: all three previous implementations agreed here ---
		{in: "a", want: []Segment{k("a")}},
		{in: "a.b.c", want: []Segment{k("a"), k("b"), k("c")}},
		{in: "spec.auth.secretRef.name", want: []Segment{k("spec"), k("auth"), k("secretRef"), k("name")}},

		// --- quoted keys: REJECTED by oasgen's parser, which documented them as supported ---
		{in: "a['b.c']", want: []Segment{k("a"), k("b.c")},
			why: "oasgen's parser refuses this with 'invalid characters in segment' while its doc comment names it as supported"},
		{in: `a["b.c"]`, want: []Segment{k("a"), k("b.c")}},
		{in: "a['b.c'].d", want: []Segment{k("a"), k("b.c"), k("d")}},
		{in: "['a..b']", want: []Segment{k("a..b")},
			why: "a quoted key may contain the sequence that is invalid unquoted"},

		// --- indices: the pair that motivated Kind ---
		{in: "a[0]", want: []Segment{k("a"), i(0)},
			why: "UNQUOTED digits are an index; the old parser returned \"0\" and lost that"},
		{in: "a['0']", want: []Segment{k("a"), k("0")},
			why: "QUOTED digits are a map key. Same flat form as a[0]; different segment"},
		{in: `a["0"]`, want: []Segment{k("a"), k("0")}},
		{in: "a.0", want: []Segment{k("a"), k("0")},
			why: "a bare dot-separated token is always a KEY, even if it looks like a number"},
		{in: "credentials[0]", want: []Segment{k("credentials"), i(0)}},
		{in: "spec.credentials[0].secretName",
			want: []Segment{k("spec"), k("credentials"), i(0), k("secretName")},
			why:  "the #510 path: resolves in rest-dynamic-controller, refused by oasgen's parser"},
		{in: "spec.items[12].key", want: []Segment{k("spec"), k("items"), i(12), k("key")}},
		{in: "a['x'].y[3].z", want: []Segment{k("a"), k("x"), k("y"), i(3), k("z")}},
		{in: "[0]", want: []Segment{i(0)}},
		{in: "a[0][1]", want: []Segment{k("a"), i(0), i(1)},
			why: "adjacent bracket groups with no separating dot"},

		// --- index forms that were already refused, and must stay refused ---
		{in: "a[01]", err: "bracket must contain",
			why: "leading zeros are the shape of a quoted key written without quotes; guessing is the content sniffing this package removes"},
		{in: "a[-1]", err: "bracket must contain"},
		{in: "a[x]", err: "bracket must contain"},

		// --- predicates: an UNDOCUMENTED form, present in the walker only ---
		{in: "a[?key=value]", want: []Segment{k("a"), p("key", "value")}},
		{in: "credentials[?name=admin].value",
			want: []Segment{k("credentials"), p("name", "admin"), k("value")}},
		{in: "a[?k=]", want: []Segment{k("a"), p("k", "")},
			why: "an empty VALUE is accepted, matching the grammar this replaces"},
		{in: "a[?=v]", err: "non-empty key",
			why: "an empty KEY is rejected -- the asymmetry is inherited deliberately"},
		{in: "a.[?k=v]", want: []Segment{k("a"), p("k", "v")},
			why: "a bracket group after a dot. Accepted for COMPATIBILITY: '.id'-style paths exist in the wild because two documented dialects disagreed"},

		// --- THE CORRECTION: quoting now escapes a predicate ---
		{in: "a['?k=v']", want: []Segment{k("a"), k("?k=v")},
			why: "the old parser produced the same segment as a[?k=v], so the walker's content sniffing read a QUOTED LITERAL as a selector. An escape that does not escape is worse than no escape"},

		// --- malformed ---
		{in: "", err: "empty"},
		{in: ".", err: "nothing but separators"},
		{in: "trailing.", err: "trailing separator"},
		{in: "a..b", err: "empty segment"},
		{in: "a[]", err: "empty bracket"},
		{in: "a['']", want: []Segment{k("a"), k("")},
			why: "an explicitly quoted empty key is a key; a[] is not"},
		{in: "a['unterminated", err: "unterminated"},
		{in: "a[0", err: "unterminated bracket"},
		{in: ".leadingDot", want: []Segment{k("leadingDot")},
			why: "leading dots are stripped, for the same compatibility reason as a.[?k=v]"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.err != "" {
				require.Error(t, err, "must be refused: %s", tc.why)
				assert.Contains(t, err.Error(), tc.err)
				return
			}
			require.NoError(t, err, tc.why)
			assert.Equal(t, tc.want, got, tc.why)
		})
	}
}

// TestTheTwoCollapsingPairsAreNowDistinct is the whole reason Kind exists, asserted on its own so it
// cannot be lost in the table.
func TestTheTwoCollapsingPairsAreNowDistinct(t *testing.T) {
	for _, pair := range [][2]string{
		{"a[0]", "a['0']"},
		{"a[?k=v]", "a['?k=v']"},
	} {
		left, err := Parse(pair[0])
		require.NoError(t, err)
		right, err := Parse(pair[1])
		require.NoError(t, err)

		assert.NotEqual(t, left, right, "%s and %s must parse to DIFFERENT segments", pair[0], pair[1])
		assert.Equal(t, Keys(left), Keys(right),
			"...while their FLAT forms stay identical -- which is why the flat form could not carry the "+
				"distinction, and why Keys() is documented as lossy")
	}
}

// TestKeysReproducesTheOldFlatForm pins the compatibility shim. Every pre-existing caller took []string,
// so Keys must produce exactly what the previous parser did.
func TestKeysReproducesTheOldFlatForm(t *testing.T) {
	for in, want := range map[string][]string{
		"a.b.c":                          {"a", "b", "c"},
		"a['b.c']":                       {"a", "b.c"},
		"spec.credentials[0].secretName": {"spec", "credentials", "0", "secretName"},
		"a['x'].y[3].z":                  {"a", "x", "y", "3", "z"},
		"credentials[?name=admin].value": {"credentials", "?name=admin", "value"},
	} {
		segs, err := Parse(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, Keys(segs), "Keys(%q)", in)
	}
}

// TestEveryParsedPathRoundTrips -- String must emit something Parse accepts, and to the same segments.
// A round trip is what a writer needs: a path it constructed must be re-readable.
func TestEveryParsedPathRoundTrips(t *testing.T) {
	for _, in := range []string{
		"a", "a.b.c", "a['b.c']", "a[0]", "a['0']", "a[?k=v]", "a['?k=v']", "a[?k=]",
		"spec.credentials[0].secretName", "a['x'].y[3].z", "[0]", "a['']", "a..b",
	} {
		first, err := Parse(in)
		if err != nil {
			continue // malformed inputs have nothing to round-trip
		}
		var b strings.Builder
		for n, s := range first {
			str := s.String()
			if n > 0 && !strings.HasPrefix(str, "[") {
				b.WriteByte('.')
			}
			b.WriteString(str)
		}
		again, err := Parse(b.String())
		require.NoError(t, err, "rendered %q from %q and it did not parse", b.String(), in)
		assert.Equal(t, first, again, "%q rendered as %q parsed to different segments", in, b.String())
	}
}

// TestKindIsNeverDecidedByContent is the invariant, stated as a test so a future change that reintroduces
// sniffing fails here rather than in a walker.
func TestKindIsNeverDecidedByContent(t *testing.T) {
	// A quoted key whose CONTENT looks like every other form must still be a key.
	for _, in := range []string{"a['0']", "a['?k=v']", "a['[0]']", "a['a.b']", "a['']"} {
		segs, err := Parse(in)
		require.NoError(t, err, in)
		require.Len(t, segs, 2, in)
		assert.Equal(t, KindKey, segs[1].Kind,
			"%s is QUOTED, so it is a key whatever its content says", in)
	}
}

// TestQuotedKeysRoundTrip covers what TestEveryParsedPathRoundTrips did not: keys that contain a quote
// character. String must quote with the OTHER character, or its output does not parse.
func TestQuotedKeysRoundTrip(t *testing.T) {
	for _, key := range []string{"it's", `say "hi"`, "a.b", "[0]", "?k=v", ""} {
		rendered := Segment{Kind: KindKey, Key: key}.String()
		segs, err := Parse("x" + rendered)
		if !strings.HasPrefix(rendered, "[") {
			segs, err = Parse("x." + rendered)
		}
		require.NoError(t, err, "key %q rendered as %q and did not parse", key, rendered)
		assert.Equal(t, []Segment{k("x"), k(key)}, segs, "key %q rendered as %q", key, rendered)
	}
}

// TestTheGrammarsLimits pins the package doc's "Limits": quote characters toggle, with no escape.
func TestTheGrammarsLimits(t *testing.T) {
	// Refused, never mis-parsed.
	for _, in := range []string{
		"a[?name=O'Brien]", // an unbalanced quote leaves the bracket unterminated
		"a[?expr=x]y]",     // a ']' in a predicate value closes the bracket early
	} {
		_, err := Parse(in)
		assert.Error(t, err, "%q must be refused", in)
	}

	// Balanced quotes in a predicate value are accepted and KEPT.
	segs, err := Parse(`a[?name=say "hi"]`)
	require.NoError(t, err)
	assert.Equal(t, []Segment{k("a"), p("name", `say "hi"`)}, segs)

	// A key with both quote characters round-trips when its own quote character is balanced...
	balanced := `it's "x"`
	segs, err = Parse("x" + Segment{Kind: KindKey, Key: balanced}.String())
	require.NoError(t, err)
	assert.Equal(t, []Segment{k("x"), k(balanced)}, segs)

	// ...and cannot be written when it is not: the rendering exists, and Parse refuses it.
	odd := `it's a"b`
	_, err = Parse("x" + Segment{Kind: KindKey, Key: odd}.String())
	assert.Error(t, err, "a key with a ' and an odd number of \" has no rendering that parses")
}
