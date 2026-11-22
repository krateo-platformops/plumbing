package valuesschema

import (
	"reflect"
	"testing"
)

// An empty list emits no `items`, so its items accept any value, and the report says so at
// "path[]" like every other any-value path (core-provider#568's cross-check counts on it). The
// schema is unchanged: the golden schema files, written before this finding existed, still match
// byte for byte (TestInfer_Golden), so only the report gained the entry.
func TestInfer_EmptyListItemsAreReportedUntyped(t *testing.T) {
	r := infer(t, "basic")
	s := decode(t, r.Schema)
	if n := at(t, s, "tolerations"); !reflect.DeepEqual(n, map[string]any{"type": "array", "default": []any{}}) {
		t.Errorf("tolerations ([]) = %v, want exactly {type: array, default: []}", n)
	}
	var got []Finding
	for _, f := range r.Report.Findings {
		if f.Path == "tolerations[]" || f.Path == "tolerations" {
			got = append(got, f)
		}
	}
	want := []Finding{{Path: "tolerations[]", Kind: KindUntyped, Detail: "empty list"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("findings for tolerations = %v, want %v", got, want)
	}
	// Control: a list with a typed element reports nothing for its items.
	for _, f := range r.Report.Findings {
		if f.Path == "args[]" {
			t.Errorf("args[] (a list of strings) was reported: %v", f)
		}
	}
}
