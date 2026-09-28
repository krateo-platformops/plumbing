package crdgen

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func obj(props map[string]apiextensionsv1.JSONSchemaProps, required ...string) apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{Type: "object", Properties: props, Required: required}
}

func str(def string) apiextensionsv1.JSONSchemaProps {
	p := apiextensionsv1.JSONSchemaProps{Type: "string"}
	if def != "" {
		p.Default = &apiextensionsv1.JSON{Raw: []byte(`"` + def + `"`)}
	}
	return p
}

func defaultOf(t *testing.T, s apiextensionsv1.JSONSchemaProps, path ...string) string {
	t.Helper()
	cur := s
	for _, p := range path {
		next, ok := cur.Properties[p]
		if !ok {
			t.Fatalf("no property %q", p)
		}
		cur = next
	}
	if cur.Default == nil {
		return ""
	}
	return string(cur.Default.Raw)
}

// The regression for core-provider#138 (originally #235).
//
// Kubernetes applies a default only when the field's PARENT exists in the submitted object. So an
// optional parent whose child has a default produced a CRD where omitting the parent silently
// dropped the child's default — the author had to write `image: {}` to get a default they had
// already declared.
func TestOptionalParentWithDefaultedChildIsMaterialized(t *testing.T) {
	spec := obj(map[string]apiextensionsv1.JSONSchemaProps{
		"image": obj(map[string]apiextensionsv1.JSONSchemaProps{
			"tag": str("latest"),
		}),
	})

	materializeDefaultedParents(&spec)

	if got := defaultOf(t, spec, "image"); got != "{}" {
		t.Errorf("optional parent with a defaulted child must get default:{} so the apiserver "+
			"materializes it; got %q", got)
	}
	if got := defaultOf(t, spec, "image", "tag"); got != `"latest"` {
		t.Errorf("the child's own default must be untouched, got %q", got)
	}
}

// Nested two deep: the inner parent is decided first, then the outer one.
func TestMaterializationIsRecursive(t *testing.T) {
	spec := obj(map[string]apiextensionsv1.JSONSchemaProps{
		"outer": obj(map[string]apiextensionsv1.JSONSchemaProps{
			"inner": obj(map[string]apiextensionsv1.JSONSchemaProps{
				"tag": str("latest"),
			}),
		}),
	})

	materializeDefaultedParents(&spec)

	if got := defaultOf(t, spec, "outer"); got != "{}" {
		t.Errorf("outer parent must be materialized, got %q", got)
	}
	if got := defaultOf(t, spec, "outer", "inner"); got != "{}" {
		t.Errorf("inner parent must be materialized, got %q", got)
	}
}

// The guard that matters most: materializing must never make a previously-valid object invalid.
func TestParentWithUnsatisfiableRequiredChildIsLeftAlone(t *testing.T) {
	spec := obj(map[string]apiextensionsv1.JSONSchemaProps{
		// Optional, but requires a child that has no default. `{}` would fail its own validation,
		// so a composition that was valid by omitting `guarded` would start being REJECTED.
		"guarded": obj(map[string]apiextensionsv1.JSONSchemaProps{
			"mandatory": str(""),
			"tag":       str("latest"),
		}, "mandatory"),
	})

	materializeDefaultedParents(&spec)

	if got := defaultOf(t, spec, "guarded"); got != "" {
		t.Errorf("a parent whose required child has no default must NOT be materialized — default:{} "+
			"would be invalid against its own schema and would reject compositions that are valid "+
			"today by omission; got %q", got)
	}
}

// A required child that has a default is STILL not safe, and this is the case that broke production
// (core-provider#142).
//
// The first version of this guard assumed the default would be filled in, so `{}` would end up
// valid. It does not: the apiserver validates a default LITERALLY against its own schema and does
// not apply nested defaults to it first, so `{}` fails `required` regardless.
func TestParentWithDefaultedRequiredChildIsNotMaterialized(t *testing.T) {
	spec := obj(map[string]apiextensionsv1.JSONSchemaProps{
		"image": obj(map[string]apiextensionsv1.JSONSchemaProps{
			"tag": str("latest"),
		}, "tag"),
	})

	materializeDefaultedParents(&spec)

	if got := defaultOf(t, spec, "image"); got != "" {
		t.Errorf("an object with ANY required property cannot be defaulted to {}: the apiserver "+
			"rejects the CRD with \"tag: Required value\" even though tag has a default; got %q", got)
	}
}

// The exact production shape from core-provider#142: builder-publish's configurationRef, nested one
// level under an optional parent, requiring "name".
//
// Kept as a named fixture rather than folded into the case above because the nesting mattered to the
// diagnosis — the first hypothesis was that the guard only inspected top-level parents. It did not;
// the walk was always recursive, and the depth was a red herring. The real fault was the guard's
// premise.
func TestBuilderPublishConfigurationRefIsNotMaterialized(t *testing.T) {
	cfgRef := obj(map[string]apiextensionsv1.JSONSchemaProps{
		"name":      str("x"),
		"namespace": str(""),
	}, "name")

	spec := obj(map[string]apiextensionsv1.JSONSchemaProps{
		"repository":  obj(map[string]apiextensionsv1.JSONSchemaProps{"configurationRef": cfgRef}),
		"pullRequest": obj(map[string]apiextensionsv1.JSONSchemaProps{"configurationRef": cfgRef}),
	})

	materializeDefaultedParents(&spec)

	for _, parent := range []string{"repository", "pullRequest"} {
		if got := defaultOf(t, spec, parent, "configurationRef"); got != "" {
			t.Errorf("%s.configurationRef must not be materialized — this exact shape produced "+
				"\"default.name: Required value\" and made the whole generated CRD invalid, so the "+
				"CompositionDefinition could not sync at all; got %q", parent, got)
		}
		// The parent itself must also stay bare: its only descendant default is unreachable now.
		if got := defaultOf(t, spec, parent); got != "" {
			t.Errorf("%s must not be materialized either; got %q", parent, got)
		}
	}
}

// No default anywhere beneath it: materializing would turn an absent field into a present-but-empty
// one for no benefit, changing what `has(spec.x)` means for anything reading the object.
func TestParentWithNoDefaultsBeneathIsLeftAlone(t *testing.T) {
	spec := obj(map[string]apiextensionsv1.JSONSchemaProps{
		"plain": obj(map[string]apiextensionsv1.JSONSchemaProps{
			"tag": str(""),
		}),
	})

	materializeDefaultedParents(&spec)

	if got := defaultOf(t, spec, "plain"); got != "" {
		t.Errorf("nothing beneath is defaulted, so there is nothing to materialize for; got %q", got)
	}
}

// An author-supplied default wins — we never overwrite an explicit choice.
func TestExplicitParentDefaultIsPreserved(t *testing.T) {
	explicit := obj(map[string]apiextensionsv1.JSONSchemaProps{"tag": str("latest")})
	explicit.Default = &apiextensionsv1.JSON{Raw: []byte(`{"tag":"pinned"}`)}
	spec := obj(map[string]apiextensionsv1.JSONSchemaProps{"image": explicit})

	materializeDefaultedParents(&spec)

	if got := defaultOf(t, spec, "image"); got != `{"tag":"pinned"}` {
		t.Errorf("an explicit parent default must not be overwritten, got %q", got)
	}
}

// A required parent is always present, so its children's defaults already apply; stamping one would
// be noise.
func TestRequiredParentIsNotMaterialized(t *testing.T) {
	spec := obj(map[string]apiextensionsv1.JSONSchemaProps{
		"image": obj(map[string]apiextensionsv1.JSONSchemaProps{"tag": str("latest")}),
	}, "image")

	materializeDefaultedParents(&spec)

	if got := defaultOf(t, spec, "image"); got != "" {
		t.Errorf("a required parent is always present; no default needed, got %q", got)
	}
}
