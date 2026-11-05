package maps

import "testing"

// A MISSING KEY IS NOT FOUND, at every depth. NestedValue used to drop nestedFieldNoCopy's found
// flag, so a missing LAST key returned (nil, true) and NestedString reported an access error for a
// key that simply was not there. Rows span the input STATES of a two-level path, not the code arms:
// the parent missing, the leaf missing (the defect), the leaf set (the control), the leaf explicitly
// null, and the parent of the wrong type.
func TestNestedValueAndNestedStringOnMissingKeys(t *testing.T) {
	cases := []struct {
		name      string
		obj       map[string]any
		wantVal   any
		wantFound bool
		wantStr   string
		wantErr   bool
	}{
		{"parent missing", map[string]any{}, nil, false, "", false},
		{"parent present, leaf missing — THE DEFECT", map[string]any{"status": map[string]any{"conditions": []any{}}}, nil, false, "", false},
		{"leaf set — the control", map[string]any{"status": map[string]any{"digest": "abc"}}, "abc", true, "abc", false},
		// An explicit null is a value of the wrong type, as in k8s's unstructured.NestedString. Kept
		// an error deliberately: this fix is about ABSENT keys, not about coercing a present null.
		{"leaf explicitly null", map[string]any{"status": map[string]any{"digest": nil}}, nil, true, "", true},
		{"parent is not a map", map[string]any{"status": "ready"}, nil, false, "", false},
	}
	for _, c := range cases {
		v, found := NestedValue(c.obj, []string{"status", "digest"})
		if found != c.wantFound || v != c.wantVal {
			t.Errorf("%s: NestedValue = (%v, %v), want (%v, %v)", c.name, v, found, c.wantVal, c.wantFound)
		}
		s, err := NestedString(c.obj, "status", "digest")
		if (err != nil) != c.wantErr || s != c.wantStr {
			t.Errorf("%s: NestedString = (%q, %v), want (%q, err=%v)", c.name, s, err, c.wantStr, c.wantErr)
		}
	}
	t.Logf("%d rows", len(cases))
}

// Arrays keep their semantics: an index past the end is not found, an index inside is.
func TestNestedValueArrayIndexes(t *testing.T) {
	obj := map[string]any{"items": []any{map[string]any{"name": "a"}}}
	if v, found := NestedValue(obj, []string{"items", "0", "name"}); !found || v != "a" {
		t.Errorf("items.0.name = (%v, %v), want (a, true)", v, found)
	}
	if _, found := NestedValue(obj, []string{"items", "1", "name"}); found {
		t.Error("items.1.name was found in a one-element array")
	}
	if _, found := NestedValue(obj, []string{"items", "0", "missing"}); found {
		t.Error("items.0.missing was found: a missing leaf under an array element must not be found")
	}
}
