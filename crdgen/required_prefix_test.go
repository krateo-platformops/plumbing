package crdgen

import (
	"strings"
	"testing"
)

// plumbing#61: a required list leaked from one node onto a sibling whose property name it is a
// prefix of. resourcesRefs.items[] declares required [id]; resourcesRefsTemplate[] declares none,
// yet came out requiring id, so the apiserver refused every object carrying a template. It no
// longer reproduces (measured on v1.86.0; no commit names #61), so this pins it, with the issue's
// reproducer verbatim: kind/version/spec at the root. The control renames the two properties so
// neither is a prefix of the other, and must come out the same.
const requiredPrefixShape = `{"type":"object","properties":{"kind":{"type":"string","default":"ReproA"},"version":{"type":"string","default":"v1beta1"},"spec":{"type":"object","properties":{"resourcesRefs":{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","required":["id"],"properties":{"apiVersion":{"type":"string"},"id":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"},"resource":{"type":"string"}}}}}},"resourcesRefsTemplate":{"type":"array","items":{"type":"object","properties":{"iterator":{"type":"string"},"template":{"type":"object","properties":{"apiVersion":{"type":"string"},"id":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"},"resource":{"type":"string"}}}}}}}}}}`

func TestRequiredDoesNotLeakOntoAPrefixedSibling(t *testing.T) {
	control := strings.NewReplacer(`"resourcesRefsTemplate"`, `"bravoTemplate"`, `"resourcesRefs"`, `"alphaRefs"`).Replace(requiredPrefixShape)
	for _, tc := range []struct{ name, schema, refs, tmpl string }{
		{"prefixed names", requiredPrefixShape, "resourcesRefs", "resourcesRefsTemplate"},
		{"control: unrelated names", control, "alphaRefs", "bravoTemplate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crd := generateV1(t, tc.schema)
			spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["spec"]
			tmpl := spec.Properties[tc.tmpl]
			if tmpl.Items == nil || tmpl.Items.Schema == nil || len(tmpl.Items.Schema.Properties) == 0 {
				t.Fatalf("%s[] is missing from the CRD: the test cannot see a leak", tc.tmpl)
			}
			if r := tmpl.Items.Schema.Required; len(r) != 0 {
				t.Errorf("%s[] requires %v, which its source never declared", tc.tmpl, r)
			}
			if r := tmpl.Items.Schema.Properties["template"].Required; len(r) != 0 {
				t.Errorf("%s[].template requires %v, which its source never declared", tc.tmpl, r)
			}
			// The legitimate required stays where it was declared.
			refs := spec.Properties[tc.refs].Properties["items"]
			if refs.Items == nil || refs.Items.Schema == nil || strings.Join(refs.Items.Schema.Required, ",") != "id" {
				t.Errorf("%s.items[] lost its declared required [id]", tc.refs)
			}
			// Through the apiserver's own validator: a template item without id is admitted, and a
			// resourcesRefs item without id is refused.
			ok := probe("spec", map[string]any{tc.tmpl: []any{map[string]any{"iterator": "x", "template": map[string]any{"name": "n"}}}})
			if errs := admit(t, crd, ok); len(errs) != 0 {
				t.Errorf("a %s item without id was refused: %v", tc.tmpl, errs)
			}
			bad := probe("spec", map[string]any{tc.refs: map[string]any{"items": []any{map[string]any{"name": "n"}}}})
			if errs := admit(t, crd, bad); len(errs) == 0 {
				t.Errorf("a %s item without id was admitted: the declared required is not enforced", tc.refs)
			}
		})
	}
}
