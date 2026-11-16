package fieldpath

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustParse(t *testing.T, p string) []Segment {
	t.Helper()
	segs, err := Parse(p)
	require.NoError(t, err, "fixture path %q must parse", p)
	return segs
}

// fromJSON builds a document the way a request body arrives: maps, slices, float64 numbers.
func fromJSON(t *testing.T, s string) any {
	t.Helper()
	if s == "" {
		return nil
	}
	var v any
	require.NoError(t, json.Unmarshal([]byte(s), &v))
	return v
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// TestSetRules pins every rule decided on plumbing#111, each row starting from its own document.
func TestSetRules(t *testing.T) {
	for _, tc := range []struct {
		name, start, path string
		value             any
		want              string // the document after the write, as JSON
		errIs             string // non-empty: refused, with this in the message
	}{
		// --- keys ---
		{name: "replace an existing key", start: `{"a":{"b":1}}`, path: "a.b", value: 2, want: `{"a":{"b":2}}`},
		{name: "an absent key creates a map for the next key", start: `{}`, path: "a.b.c", value: "v", want: `{"a":{"b":{"c":"v"}}}`},
		{name: "an absent key creates an array for the next index", start: `{}`, path: "a[0].b", value: "v", want: `{"a":[{"b":"v"}]}`},
		{name: "core-provider (1): a quoted key holding the separator and brackets is ONE key",
			start: `{}`, path: `spec['a.b[0]'].c`, value: 1, want: `{"spec":{"a.b[0]":{"c":1}}}`},

		// --- indices ---
		{name: "replace an element", start: `{"l":["x","y"]}`, path: "l[1]", value: "z", want: `{"l":["x","z"]}`},
		{name: "== len appends", start: `{"l":["x"]}`, path: "l[1]", value: "y", want: `{"l":["x","y"]}`},
		{name: "core-provider (2): > len is refused, never padded", start: `{"l":["x"]}`, path: "l[3]", value: "y",
			errIs: "past the end of an array of 1; a write appends at index 1 and never pads"},
		{name: "an index into a created array must be 0", start: `{}`, path: "l[1].x", value: 1,
			errIs: "past the end of an array of 0"},

		// --- selectors ---
		{name: "a selector writes into the one element it matches",
			start: `{"c":[{"type":"password"},{"type":"otp"}]}`, path: "c[?type=otp].value", value: "s",
			want: `{"c":[{"type":"password"},{"type":"otp","value":"s"}]}`},
		{name: "a selector as the last segment replaces the element",
			start: `{"c":[{"type":"otp","n":1}]}`, path: "c[?type=otp]", value: map[string]any{"type": "otp", "n": 2},
			want: `{"c":[{"n":2,"type":"otp"}]}`},
		{name: "a selector that matches nothing is refused", start: `{"c":[{"type":"password"}]}`, path: "c[?type=otp].value", value: "s",
			errIs: "matches no element, and a write never invents one"},
		{name: "an ambiguous selector is refused, as in Get", start: `{"c":[{"type":"x"},{"type":"x"}]}`, path: "c[?type=x].v", value: 1,
			errIs: "matches more than one element"},
		{name: "a selector into an absent array is refused", start: `{}`, path: "c[?type=otp].value", value: "s",
			errIs: "a write never invents the element a selector names"},

		// --- contradictions and scalars ---
		{name: "a key on an array is refused", start: `{"l":[{"a":1}]}`, path: "l['0'].a", value: 2, errIs: "a map key cannot address an array element"},
		{name: "an index on a map is refused", start: `{"m":{"0":1}}`, path: "m[0]", value: 2, errIs: "an array index cannot address a map"},
		{name: "a selector on a map is refused", start: `{"m":{"type":"x"}}`, path: "m[?type=x].v", value: 2, errIs: "selector cannot address a map"},
		{name: "core-provider (3): a scalar where a container is needed is refused, not overwritten",
			start: `{"a":"scalar"}`, path: "a.b", value: 1, errIs: "string is neither a map nor an array; a write does not overwrite it"},
		{name: "a null where a container is needed is refused, as Get refuses to read through it",
			start: `{"a":null}`, path: "a.b", value: 1, errIs: "<nil> is neither a map nor an array"},

		// --- the root ---
		{name: "a nil root is created by the first key", start: "", path: "a", value: 1, want: `{"a":1}`},
		{name: "a nil root is created by the first index", start: "", path: "[0].a", value: 1, want: `[{"a":1}]`},
		{name: "an append to a root array replaces the root", start: `[1]`, path: "[1]", value: 2, want: `[1,2]`},
		{name: "a nil root cannot be created by a selector", start: "", path: "[?a=b].c", value: 1, errIs: "never invents"},
		{name: "a nil root stays nil when the write is refused after creating it", start: "", path: "a[?k=v].c", value: 1, errIs: "never invents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := fromJSON(t, tc.start)
			before := asJSON(t, obj)
			out, err := Set(obj, mustParse(t, tc.path), tc.value)
			if tc.errIs != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errIs)
				assert.Equal(t, before, asJSON(t, obj), "a refused write must change nothing")
				assert.Equal(t, before, asJSON(t, out), "a refused write returns the document it was given")
				return
			}
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, asJSON(t, out))
		})
	}
}

// A refused write is ATOMIC: a path refused at its last segment, after crossing absent keys that a
// successful write would have created, leaves none of them behind.
func TestSetIsAtomic(t *testing.T) {
	obj := fromJSON(t, `{"keep":1,"l":["x"]}`)
	for _, p := range []string{"new.deeper.l[5]", "l[0].a", "new[0].m[?k=v].x", "fresh.l[2]"} {
		_, err := Set(obj, mustParse(t, p), "v")
		require.Error(t, err, p)
		assert.JSONEq(t, `{"keep":1,"l":["x"]}`, asJSON(t, obj), "after the refused write of %s", p)
	}
}

// The live shapes rest-dynamic-controller builds request bodies with, from oasgen-provider's
// refinement on #111, including the migration note: with no padding, an out-of-order index
// declaration is refused where the flat writer padded, and reordering it is the fix.
func TestRestDynamicControllerBodyShapes(t *testing.T) {
	var body any
	var err error
	body, err = Set(body, mustParse(t, "credentials[0].value"), "pw")
	require.NoError(t, err)
	body, err = Set(body, mustParse(t, "credentials[1].secretData"), "otp")
	require.NoError(t, err)
	assert.JSONEq(t, `{"credentials":[{"value":"pw"},{"secretData":"otp"}]}`, asJSON(t, body))

	_, err = Set(nil, mustParse(t, "credentials[1].x"), "out-of-order")
	require.Error(t, err, "an index declared before the one it follows is refused, not padded")
	assert.Contains(t, err.Error(), "never pads")
}

// A BARE numeric segment is a KEY in this dialect, because Parse types it so (#92: segments are typed
// by how they were written): `credentials.0.value` addresses the map key "0". So on an empty body it
// creates {"credentials":{"0":...}}, and against an existing array it is refused, exactly as Get refuses
// to read it. The flat writer this replaces guessed an array from the digits; a caller migrating from
// it writes `credentials[0].value`. Pinned so the difference is a stated rule, not a surprise.
func TestABareNumericSegmentIsAKey(t *testing.T) {
	segs := mustParse(t, "credentials.0.value")
	require.Equal(t, KindKey, segs[1].Kind, "the parser types a bare 0 as a key")

	out, err := Set(nil, segs, "pw")
	require.NoError(t, err)
	assert.JSONEq(t, `{"credentials":{"0":{"value":"pw"}}}`, asJSON(t, out))

	_, err = Set(fromJSON(t, `{"credentials":[{"value":"old"}]}`), segs, "pw")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a map key cannot address an array element; use [0] without quotes")
}

// core-provider (5): the writer and the reader agree on what every path in Get's own table means.
// Get errors -> Set errors. Get finds a value -> Get reads back exactly what Set wrote. Get finds
// nothing -> Set either refuses or writes something Get then reads back.
func TestSetRoundTripsThroughGetsTable(t *testing.T) {
	sentinel := map[string]any{"written-by": "Set"}
	for _, row := range walkTable {
		t.Run(row.path, func(t *testing.T) {
			segs := mustParse(t, row.path)
			obj := doc()
			out, err := Set(obj, segs, sentinel)
			switch {
			case row.errIs != "":
				require.Error(t, err, "Get refuses %s (%s); Set must refuse it too", row.path, row.errIs)
				return
			case row.found:
				require.NoError(t, err, "Get reads %s; Set must write it", row.path)
			default:
				if err != nil {
					return // absent for Get, refused for Set: the index-past-end and no-match rules
				}
			}
			got, found, err := Get(out, segs)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, sentinel, got)
		})
	}
}

// core-provider (4) and the three-way contract: removing a present value, an absent one, and one the
// path cannot apply to are three distinguishable results.
func TestRemove(t *testing.T) {
	for _, tc := range []struct {
		name, start, path, want string
		found                   bool
		errIs                   string
	}{
		{name: "a present key", start: `{"a":{"b":1,"c":2}}`, path: "a.b", want: `{"a":{"c":2}}`, found: true},
		{name: "an absent key is a no-op", start: `{"a":{}}`, path: "a.b", want: `{"a":{}}`},
		{name: "an absent parent is a no-op", start: `{}`, path: "a.b.c", want: `{}`},
		{name: "an element shifts the rest", start: `{"l":[1,2,3]}`, path: "l[1]", want: `{"l":[1,3]}`, found: true},
		{name: "past the end is a no-op, as Get reads it", start: `{"l":[1]}`, path: "l[5]", want: `{"l":[1]}`},
		{name: "a selected element", start: `{"c":[{"t":"a"},{"t":"b"}]}`, path: "c[?t=a]", want: `{"c":[{"t":"b"}]}`, found: true},
		{name: "beneath a selected element", start: `{"c":[{"t":"a","v":1}]}`, path: "c[?t=a].v", want: `{"c":[{"t":"a"}]}`, found: true},
		{name: "beneath a selected element that is not the first", start: `{"c":[{"t":"a","v":1},{"t":"b","v":2}]}`, path: "c[?t=b].v",
			want: `{"c":[{"t":"a","v":1},{"t":"b"}]}`, found: true},
		{name: "a refusal at the root returns the root", start: `[1]`, path: "a", errIs: "a map key cannot address an array element"},
		{name: "a selector matching nothing is a no-op", start: `{"c":[{"t":"a"}]}`, path: "c[?t=z]", want: `{"c":[{"t":"a"}]}`},
		{name: "an ambiguous selector is an error", start: `{"c":[{"t":"a"},{"t":"a"}]}`, path: "c[?t=a]", errIs: "matches more than one element"},
		{name: "a key on an array is an error", start: `{"l":[1]}`, path: "l['0']", errIs: "a map key cannot address an array element"},
		{name: "through a scalar is an error", start: `{"a":"s"}`, path: "a.b", errIs: "neither a map nor an array"},
		{name: "through a null is an error", start: `{"a":null}`, path: "a.b", errIs: "neither a map nor an array"},
		{name: "a nil document is a no-op", start: "", path: "a", want: `null`},
		{name: "an element of a root array", start: `[1,2]`, path: "[0]", want: `[2]`, found: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := fromJSON(t, tc.start)
			before := asJSON(t, obj)
			out, found, err := Remove(obj, mustParse(t, tc.path))
			if tc.errIs != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errIs)
				assert.False(t, found, "found must be false alongside an error")
				assert.Equal(t, before, asJSON(t, obj), "a refused removal must change nothing")
				assert.Equal(t, before, asJSON(t, out), "a refused removal returns the document it was given")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.found, found)
			assert.JSONEq(t, tc.want, asJSON(t, out))
		})
	}
}

func TestAnEmptyPathIsRefused(t *testing.T) {
	_, err := Set(map[string]any{}, nil, 1)
	assert.True(t, errors.Is(err, ErrEmptyPath))
	_, _, err = Remove(map[string]any{}, nil)
	assert.True(t, errors.Is(err, ErrEmptyPath))
}

// An append never writes into the caller's backing array: a slice with spare capacity shared by two
// headers must not see the element appended through one of them.
func TestSetAppendDoesNotAlias(t *testing.T) {
	backing := make([]any, 1, 4)
	backing[0] = "x"
	wider := backing[:2] // a second header over the same backing array
	doc := map[string]any{"l": backing}

	out, err := Set(doc, mustParse(t, "l[1]"), "appended")
	require.NoError(t, err)
	assert.Equal(t, []any{"x", "appended"}, out.(map[string]any)["l"])
	assert.Nil(t, wider[1], "the append wrote into a backing array another slice shares")
}
