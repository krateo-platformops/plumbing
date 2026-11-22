package crdgen

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// core-provider#613: an untyped node accepts any value, but keeps its
// description, title, example and default. A values schema infers a decimal
// leaf untyped on purpose ({default: 0.5}); the default used to vanish with no
// warning.
func TestUntypedNodeKeepsItsDefaultAndDescription(t *testing.T) {
	const spec = `{"type":"object","properties":{
	  "ratio":{"default":0.5,"description":"the share","title":"Ratio","examples":[0.25]},
	  "anything":{},
	  "picked":{"enum":[null,1]},
	  "typed":{"type":"integer","default":3},
	  "list":{"type":"array","items":{},"default":[0.1,0.2]},
	  "tuning":{"type":"object","properties":{"ratio":{"default":0.5}}}}}`
	out, ws, err := GenerateWithDiagnostics(Options{Group: "test.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(spec)})
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties

	ratio := props["ratio"]
	if ratio.XPreserveUnknownFields == nil || !*ratio.XPreserveUnknownFields || ratio.Type != "" {
		t.Errorf("ratio is still any value: type %q, preserve-unknown %v", ratio.Type, ratio.XPreserveUnknownFields)
	}
	if ratio.Default == nil || string(ratio.Default.Raw) != "0.5" {
		t.Errorf("ratio's default = %v, want 0.5", ratio.Default)
	}
	if ratio.Description != "the share" || ratio.Title != "Ratio" {
		t.Errorf("ratio's description, title = %q, %q", ratio.Description, ratio.Title)
	}
	if ratio.Example == nil || string(ratio.Example.Raw) != "0.25" {
		t.Errorf("ratio's example = %v, want 0.25", ratio.Example)
	}

	// An untyped node with nothing to keep is the bare any value it always was.
	if a := props["anything"]; a.Default != nil || a.Description != "" || a.XPreserveUnknownFields == nil {
		t.Errorf("anything = %+v, want a bare any value", a)
	}

	// Control: a typed default is unchanged.
	if typed := props["typed"]; typed.Type != "integer" || typed.Default == nil || string(typed.Default.Raw) != "3" {
		t.Errorf("typed = %+v", typed)
	}

	// Control: a typed list of untyped decimals keeps its default (core-provider#613's probe).
	if list := props["list"]; list.Type != "array" || list.Default == nil || string(list.Default.Raw) != "[0.1,0.2]" {
		t.Errorf("list = %+v", list)
	}

	// Nested, the shape a values schema infers: the leaf keeps its default, and
	// materializeDefaultedParents counts it, so the optional parent gets
	// default {} and the apiserver reaches the leaf when tuning is omitted.
	tuning := props["tuning"]
	if tuning.Default == nil || string(tuning.Default.Raw) != "{}" {
		t.Errorf("tuning's default = %v, want {} (its untyped leaf carries a default)", tuning.Default)
	}
	if r := tuning.Properties["ratio"]; r.Default == nil || string(r.Default.Raw) != "0.5" {
		t.Errorf("tuning.ratio's default = %v, want 0.5", r.Default)
	}

	// An enum can't sit on an untyped node: dropped, and said so. Only a null first value leaves
	// one untyped (inferType types a node from its enum's first value), so that is the case here.
	for _, name := range []string{"picked"} {
		if n := props[name]; len(n.Enum) != 0 {
			t.Errorf("%s carries an enum on an untyped node, which the apiserver refuses", name)
		}
		warned := false
		for _, w := range ws {
			if w.Kind == KindConstraintDropped && strings.HasSuffix(w.Path, name) {
				warned = true
			}
		}
		if !warned {
			t.Errorf("%s's enum was dropped without a ConstraintDropped warning: %v", name, ws)
		}
	}
	for _, w := range ws {
		if strings.HasSuffix(w.Path, "ratio") {
			t.Errorf("ratio warned, but nothing of it was dropped: %v", w)
		}
	}
}
