package fieldpath

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doc is one fixture carrying every shape the table needs, so a row's outcome depends on the PATH rather
// than on which document it was given.
func doc() map[string]any {
	return map[string]any{
		"spec": map[string]any{
			"name": "n1",
			"credentials": []any{
				map[string]any{"type": "password", "valueSecretRef": map[string]any{"name": "pw-secret", "key": "password"}},
				map[string]any{"type": "otp", "secretDataSecretRef": map[string]any{"name": "otp-secret", "key": "secretData"}},
				"a-scalar-element",
				map[string]any{"type": "dup", "n": 1},
				map[string]any{"type": "dup", "n": 2},
			},
			"numeric":  []any{map[string]any{"id": 2, "hit": "int"}},
			"floaty":   []any{map[string]any{"id": 2.0, "hit": "float"}},
			"stringy":  []any{map[string]any{"id": "2", "hit": "string"}},
			"zeroKey":  map[string]any{"0": "a-map-key-named-zero"},
			"emptyVal": []any{map[string]any{"type": "", "hit": "empty"}},
			"nullRef":  nil,
		},
	}
}

// TestTheWalkTable is the single pinned table. Every row is either INHERITED from
// rest-dynamic-controller's GetNestedField -- so lifting this into plumbing changes nothing for its
// callers -- or a CORRECTION, which carries its reason.
func TestTheWalkTable(t *testing.T) {
	for _, tc := range []struct {
		path  string
		want  any
		found bool
		errIs string // non-empty: Get must error and the message must contain this
		why   string
	}{
		// --- plain traversal: INHERITED ---
		{path: "spec.name", want: "n1", found: true},
		{path: "spec.credentials[0].valueSecretRef.name", want: "pw-secret", found: true},
		{path: "spec.credentials[1].secretDataSecretRef.key", want: "secretData", found: true},

		{path: "spec.nullRef.name", errIs: "neither a map nor an array",
			why: "INHERITED: a null value in the middle of a path is an ERROR, not absent, as in " +
				"rest-dynamic-controller's GetNestedField and unstructured.NestedFieldNoCopy. Pinned because " +
				"`secretRef: null` is a common document shape, and a caller wanting 'absent' must decide that itself"},

		// --- predicates: INHERITED, ported body and comments unchanged ---
		{path: "spec.credentials[?type=password].valueSecretRef.name", want: "pw-secret", found: true,
			why: "the #510 case: a provider-side collector must select the same element the controller's resolver does"},
		{path: "spec.credentials[?type=otp].secretDataSecretRef.name", want: "otp-secret", found: true},
		{path: "spec.credentials[?type=nosuch].valueSecretRef.name", found: false,
			why: "no match is ABSENT, not an error -- which is what lets #524's guard and #510's collector agree " +
				"without coordinating: a guarded entry whose element is missing contributes no Secret name"},
		{path: "spec.credentials[?type=dup].n", errIs: "matches more than one element",
			why: "AMBIGUITY IS AN ERROR, never take-the-first. Silently picking one of several matches is the " +
				"wrong-element bug selectors exist to eliminate, and the message names BOTH indices"},

		// --- the string-form comparison: INHERITED, and the reason it exists ---
		{path: "spec.numeric[?id=2].hit", want: "int", found: true,
			why: "comparison is by STRING FORM: API bodies round-trip through JSON with inconsistent numeric " +
				"typing, and a type-sensitive match would fail for reasons the author cannot see in their YAML"},
		{path: "spec.floaty[?id=2].hit", want: "float", found: true, why: "2.0 renders as 2"},
		{path: "spec.stringy[?id=2].hit", want: "string", found: true},
		{path: "spec.emptyVal[?type=].hit", want: "empty", found: true,
			why: "an empty Value matches, because the PARSER accepts [?k=] -- the walker must not refuse what " +
				"the grammar admits, or the two disagree about the same path"},

		// --- a scalar element cannot satisfy a field selector: INHERITED ---
		{path: "spec.credentials[?type=a-scalar-element].x", found: false,
			why: "element 2 is a string; a scalar cannot satisfy a field selector, so it is skipped rather than " +
				"compared"},

		// --- out of range: INHERITED, and now a DELIBERATE decision ---
		{path: "spec.credentials[99].name", found: false,
			why: "ABSENT, not an error, decided rather than inherited. The error arm is for a path whose SHAPE " +
				"cannot apply. An index past the end FITS the shape -- it says this document has fewer " +
				"elements, the same kind of fact as a missing key, which is also absent. As an error, the same " +
				"path would error on one document and be absent on another with nothing different in the path"},
		{path: "spec.missing.deeper", found: false, why: "a missing key is absent -- the fact out-of-range is likened to"},

		// --- CORRECTIONS, only possible because the walker now sees Kind ---
		{path: "spec.zeroKey['0']", want: "a-map-key-named-zero", found: true,
			why: "CORRECTION-ADJACENT: a QUOTED numeric key against a MAP resolves, which the flat form also did " +
				"-- included so the correction below is not mistaken for refusing quoted keys everywhere"},
		{path: "spec.credentials['0'].valueSecretRef.name", errIs: "a map key cannot address an array element",
			why: "CORRECTION. With []string the walker could not tell a['0'] from a[0], so a quoted key against " +
				"an array silently INDEXED it -- the author asked for a key and got element 0. Dispatching on " +
				"Kind makes the intent decidable"},
		{path: "spec.zeroKey[0]", errIs: "an array index cannot address a map",
			why: "CORRECTION, the mirror of the above: an unquoted index against a map was previously resolved " +
				"as the key \"0\" if one happened to exist. The author wrote an index; a map has none"},
		{path: "spec.name.deeper", errIs: "is neither a map nor an array",
			why: "INHERITED: walking through a scalar is a shape error, not absence"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			segs, err := Parse(tc.path)
			require.NoError(t, err, "the PARSER must accept every row; a parse failure here is a fixture bug")

			got, found, err := Get(doc(), segs)

			if tc.errIs != "" {
				require.Error(t, err, "must be a shape/ambiguity error: %s", tc.why)
				assert.Contains(t, err.Error(), tc.errIs)
				assert.False(t, found, "found must be false alongside an error")
				return
			}
			require.NoError(t, err, tc.why)
			assert.Equal(t, tc.found, found, tc.why)
			if tc.found {
				assert.Equal(t, tc.want, got, tc.why)
			}
		})
	}
}

// TestTheThreeWayContractIsDistinguishable is the invariant the signature exists for. Without it a caller
// cannot tell "the field is not there" from "I could not read the field", and collapsing the second into
// the first is how oasgen-provider #517 produced a controller that reported a resource missing while
// holding it.
func TestTheThreeWayContractIsDistinguishable(t *testing.T) {
	segs, err := Parse("spec.credentials[?type=nosuch].x")
	require.NoError(t, err)
	_, found, err := Get(doc(), segs)
	assert.False(t, found)
	assert.NoError(t, err, "ABSENT: found=false with a nil error")

	segs, err = Parse("spec.credentials[?type=dup].n")
	require.NoError(t, err)
	_, found, err = Get(doc(), segs)
	assert.False(t, found)
	assert.Error(t, err, "NOT APPLICABLE: an error, which a caller must not read as absence")
}
