package crdgen

import (
	"strings"
	"testing"
)

// Every warning site reachable from JSON input, each asserting the KIND it reports. A site typed
// wrongly would silently mis-set a consumer's "schema fully enforced" signal, so each is pinned.
// The rows name the site by a fragment of its message.
func TestWarningSites_EveryReachableSiteHasItsKind(t *testing.T) {
	for _, tc := range []struct {
		fragment string
		schema   string
		want     WarningKind
	}{
		{"unresolved $ref", `{"type":"object","properties":{"a":{"$ref":"#/$defs/missing"}}}`, KindStructureLoosened},
		{"cycle at", `{"type":"object","$defs":{"n":{"type":"object","properties":{"next":{"$ref":"#/$defs/n"}}}},"properties":{"r":{"$ref":"#/$defs/n"}}}`, KindStructureLoosened},
		{"is a union this schema cannot express", `{"type":"object","properties":{"u":{"type":["string","boolean"]}}}`, KindAcceptanceNarrowed},
		{"properties + additionalProperties schema", `{"type":"object","properties":{"o":{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"integer"}}}}`, KindStructureLoosened},
		{"dependentRequired on non-object", `{"type":"object","properties":{"s":{"type":"string","dependentRequired":{"a":["b"]}}}}`, KindConstraintDropped},
		{"not on non-scalar node", `{"type":"object","properties":{"o":{"type":"object","not":{"required":["a"]}}}}`, KindConstraintDropped},
		{"not (non-trivial subschema)", `{"type":"object","properties":{"s":{"type":"string","not":{"properties":{"a":{"type":"string"}}}}}}`, KindConstraintDropped},
		// plumbing#102: minLength inside not is native now; exclusiveMinimum is emitted nowhere, so a not
		// carrying only it is still dropped rather than becoming not:{} (which would reject everything).
		{"not (non-trivial) unsupported", `{"type":"object","properties":{"n":{"type":"number","not":{"exclusiveMinimum":3}}}}`, KindConstraintDropped},
		{"if/then/else on non-object", `{"type":"object","properties":{"s":{"type":"string","if":{"minLength":1},"then":{"maxLength":3}}}}`, KindConstraintDropped},
		{"patternProperties unsupported", `{"type":"object","properties":{"o":{"type":"object","patternProperties":{"^x":{"type":"string"}}}}}`, KindStructureLoosened},
		{"dependentSchemas unsupported", `{"type":"object","properties":{"o":{"type":"object","dependentSchemas":{"a":{"required":["b"]}}}}}`, KindConstraintDropped},
		{"prefixItems", `{"type":"object","properties":{"l":{"type":"array","prefixItems":[{"type":"string"}],"items":{"type":"string"}}}}`, KindConstraintDropped},
		{"unevaluatedProperties", `{"type":"object","properties":{"o":{"type":"object","unevaluatedProperties":true}}}`, KindStructureLoosened},
		{"unevaluatedItems unsupported", `{"type":"object","properties":{"l":{"type":"array","items":{"type":"string"},"unevaluatedItems":false}}}`, KindConstraintDropped},
		{"given default {}", `{"type":"object","properties":{"img":{"type":"object","properties":{"tag":{"type":"string","default":"x"}}}}}`, KindParentDefaulted},
	} {
		t.Run(tc.fragment, func(t *testing.T) {
			_, ws, err := GenerateWithDiagnostics(Options{Group: "x.io", Version: "v1", Kind: "K", SpecSchema: []byte(tc.schema)})
			if err != nil {
				t.Fatalf("generation failed: %v", err)
			}
			var found []Warning
			for _, w := range ws {
				if strings.Contains(w.Message, tc.fragment) {
					found = append(found, w)
				}
			}
			if len(found) == 0 {
				t.Fatalf("the schema did not reach the site %q, so this row tests nothing; warnings: %+v", tc.fragment, ws)
			}
			for _, w := range found {
				if w.Kind != tc.want {
					t.Errorf("site %q reports %s, want %s", tc.fragment, w.Kind, tc.want)
				}
			}
		})
	}
}
