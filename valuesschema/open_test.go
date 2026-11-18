package valuesschema

import (
	"encoding/json"
	"testing"
)

// openUnstated's walk: every position that describes the value is opened where unstated; an
// explicit additionalProperties (false or a schema) and a preserve-unknown node are left as written;
// `not` and if/then/else are not walked, because opening an object there would NARROW what the
// schema accepts.
func TestOpenUnstated_PositionsAndExceptions(t *testing.T) {
	src := `{"type":"object","properties":{
		"plain":{"type":"object","properties":{"a":{"type":"string"}}},
		"closed":{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}},
		"typed":{"type":"object","additionalProperties":{"type":"object","properties":{"b":{"type":"string"}}},"properties":{"a":{"type":"string"}}},
		"kept":{"type":"object","x-kubernetes-preserve-unknown-fields":true,"properties":{"a":{"type":"string"}}},
		"list":{"type":"array","items":{"type":"object","properties":{"c":{"type":"string"}}}},
		"union":{"allOf":[{"type":"object","properties":{"d":{"type":"string"}}}]},
		"neg":{"type":"string","not":{"type":"object","properties":{"e":{"type":"string"}}}},
		"cond":{"type":"object","properties":{"f":{"type":"string"}},"if":{"properties":{"f":{"const":"x"}}},"then":{"properties":{"g":{"type":"string"}}}},
		"ref":{"$ref":"#/$defs/r"}},
		"$defs":{"r":{"type":"object","properties":{"h":{"type":"string"}}}}}`
	tree, err := parseOrdered([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	in := &inferrer{report: Report{RulesVersion: RulesVersion}}
	out, err := marshalValue(in.openUnstated(tree, "sub", "#/properties/sub"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		keys []string
		want any // nil: key absent
	}{
		{[]string{"additionalProperties"}, true}, // the root itself
		{[]string{"properties", "plain", "additionalProperties"}, true},
		{[]string{"properties", "closed", "additionalProperties"}, false},
		{[]string{"properties", "typed", "additionalProperties", "additionalProperties"}, true},
		{[]string{"properties", "kept", "additionalProperties"}, nil},
		{[]string{"properties", "list", "items", "additionalProperties"}, true},
		{[]string{"properties", "union", "allOf", "0"}, "walked"},
		{[]string{"properties", "neg", "not", "additionalProperties"}, nil},
		{[]string{"properties", "cond", "if", "additionalProperties"}, nil},
		{[]string{"properties", "cond", "then", "additionalProperties"}, nil},
		{[]string{"$defs", "r", "additionalProperties"}, true},
	} {
		if c.want == "walked" {
			list, _ := dig(doc, "properties", "union", "allOf")
			first, _ := list.([]any)[0].(map[string]any)
			if first["additionalProperties"] != true {
				t.Errorf("allOf member not opened: %v", first)
			}
			continue
		}
		got, ok := dig(doc, c.keys...)
		if c.want == nil {
			if ok {
				t.Errorf("%v = %v, want absent", c.keys, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%v = %v, want %v", c.keys, got, c.want)
		}
	}
	// Reported once per opened node, in document order, with its pointer.
	want := []string{
		"#/properties/sub",
		"#/properties/sub/properties/plain",
		"#/properties/sub/properties/typed/additionalProperties",
		"#/properties/sub/properties/list/items",
		"#/properties/sub/properties/union/allOf/0",
		"#/properties/sub/properties/cond",
		"#/properties/sub/$defs/r",
	}
	var got []string
	for _, f := range in.report.Findings {
		if f.Kind != KindAuthoredMapOpened || f.Path != "sub" {
			t.Errorf("unexpected finding %+v", f)
		}
		got = append(got, f.Detail)
	}
	if len(got) != len(want) {
		t.Fatalf("opened %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("opened[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}
