package crdgen

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// apiserverAdmits runs what the apiserver runs on a write of spec: structural defaulting, the
// OpenAPI schema validator, then the CRD's x-kubernetes-validations with the apiserver's own CEL
// validator. admit (notnative_test.go) runs only the schema validator, which never evaluates CEL.
func apiserverAdmits(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, spec map[string]any) field.ErrorList {
	t.Helper()
	scheme := runtime.NewScheme()
	apiextinstall.Install(scheme)
	var internal apiextensions.JSONSchemaProps
	if err := scheme.Convert(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatal(err)
	}
	ss, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]any{"apiVersion": "composition.krateo.io/v1", "kind": "Probe",
		"metadata": map[string]any{"name": "p", "namespace": "default"}, "spec": spec}
	structuraldefaulting.Default(obj, ss)
	v, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatal(err)
	}
	if errs := validation.ValidateCustomResource(nil, obj, v); len(errs) > 0 {
		return errs
	}
	errs, _ := schemacel.NewValidator(ss, true, celconfig.PerCallLimit).
		Validate(context.Background(), field.NewPath("root"), ss, obj, nil, celconfig.RuntimeCELCostBudget)
	return errs
}

// generateCond generates spec and returns the CRD and its warnings; generation must succeed.
func generateCond(t *testing.T, spec string) (*apiextensionsv1.CustomResourceDefinition, []Warning) {
	t.Helper()
	out, warns, err := GenerateWithDiagnostics(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(spec)})
	if err != nil {
		t.Fatalf("generation refused: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	return &crd, warns
}

// One if/then/else, its warnings, and the objects the apiserver must admit and refuse. Every case
// has both, so a schema that refuses everything (or nothing) fails it.
type condCase struct {
	name   string
	schema string
	// dropped is the number of ConstraintDropped warnings; each must name its property.
	dropped int
	admit   []map[string]any
	refuse  []map[string]any
}

func runCondCases(t *testing.T, cases []condCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			crd, warns := generateCond(t, tc.schema)
			var dropped []Warning
			for _, w := range warns {
				switch w.Kind {
				case KindConstraintDropped:
					dropped = append(dropped, w)
				case KindParentDefaulted: // nothing lost: a parent given {} so nested defaults apply
				default:
					t.Errorf("unexpected warning: %v", w)
				}
			}
			if len(dropped) != tc.dropped {
				t.Fatalf("%d ConstraintDropped warnings, want %d: %v", len(dropped), tc.dropped, dropped)
			}
			for _, obj := range tc.admit {
				if errs := apiserverAdmits(t, crd, obj); len(errs) != 0 {
					t.Errorf("refused %v, which the schema accepts: %v", obj, errs)
				}
			}
			for _, obj := range tc.refuse {
				if errs := apiserverAdmits(t, crd, obj); len(errs) == 0 {
					t.Errorf("admitted %v, which the schema refuses", obj)
				}
			}
		})
	}
}

type m = map[string]any

// plumbing#211: a property constraint in a branch is enforced, not turned into has(). The
// reported shape: serviceAccount.name required and non-empty when create is false.
func TestConditionalPropertyConstraintsAreEnforced(t *testing.T) {
	sa := func(v m) m { return m{"serviceAccount": v} }
	runCondCases(t, []condCase{
		{
			name: "plumbing#211: minLength with required in then",
			schema: `{"type":"object","properties":{"serviceAccount":{"type":"object",
			  "properties":{"create":{"type":"boolean"},"name":{"type":"string"}},
			  "if":{"properties":{"create":{"const":false}}},
			  "then":{"required":["name"],"properties":{"name":{"minLength":1}}}}}}`,
			admit: []m{sa(m{"create": false, "name": "sa"}), sa(m{"create": true, "name": ""}), sa(m{"create": true})},
			// {} matches the if: an absent create satisfies properties.create, as in JSON Schema.
			refuse: []m{sa(m{"create": false, "name": ""}), sa(m{"create": false}), sa(m{})},
		},
		{
			name: "a branch property without required holds where it is absent",
			schema: `{"type":"object","properties":{"o":{"type":"object","required":["mode"],
			  "properties":{"mode":{"type":"string"},"name":{"type":"string"}},
			  "if":{"properties":{"mode":{"const":"named"}}},
			  "then":{"properties":{"name":{"minLength":2,"maxLength":3}}}}}}`,
			admit:  []m{{"o": m{"mode": "named"}}, {"o": m{"mode": "named", "name": "ab"}}, {"o": m{"mode": "other", "name": "a"}}},
			refuse: []m{{"o": m{"mode": "named", "name": "a"}}, {"o": m{"mode": "named", "name": "abcd"}}},
		},
		{
			name: "integer bounds, a fractional one rounded to the integer it equals",
			schema: `{"type":"object","properties":{"o":{"type":"object","required":["on"],
			  "properties":{"on":{"type":"boolean"},"replicas":{"type":"integer"}},
			  "if":{"properties":{"on":{"const":true}}},
			  "then":{"properties":{"replicas":{"minimum":2.5,"exclusiveMaximum":10}}}}}}`,
			admit:  []m{{"o": m{"on": true, "replicas": 3}}, {"o": m{"on": true, "replicas": 9}}, {"o": m{"on": false, "replicas": 1}}},
			refuse: []m{{"o": m{"on": true, "replicas": 2}}, {"o": m{"on": true, "replicas": 10}}},
		},
		{
			name: "number bounds, draft 4's boolean exclusiveMinimum",
			schema: `{"type":"object","properties":{"o":{"type":"object","required":["on"],
			  "properties":{"on":{"type":"boolean"},"ratio":{"type":"number"}},
			  "if":{"properties":{"on":{"const":true}}},
			  "then":{"properties":{"ratio":{"minimum":1,"exclusiveMinimum":true,"maximum":2.5}}}}}}`,
			admit:  []m{{"o": m{"on": true, "ratio": 1.5}}, {"o": m{"on": true, "ratio": 2.5}}},
			refuse: []m{{"o": m{"on": true, "ratio": 1.0}}, {"o": m{"on": true, "ratio": 2.6}}},
		},
		{
			name: "array minItems and maxItems",
			schema: `{"type":"object","properties":{"o":{"type":"object","required":["on"],
			  "properties":{"on":{"type":"boolean"},"hosts":{"type":"array","items":{"type":"string"}}},
			  "if":{"properties":{"on":{"const":true}}},
			  "then":{"properties":{"hosts":{"minItems":1,"maxItems":2}}}}}}`,
			admit:  []m{{"o": m{"on": true, "hosts": []any{"a"}}}, {"o": m{"on": false, "hosts": []any{}}}},
			refuse: []m{{"o": m{"on": true, "hosts": []any{}}}, {"o": m{"on": true, "hosts": []any{"a", "b", "c"}}}},
		},
		{
			name: "map minProperties",
			schema: `{"type":"object","properties":{"o":{"type":"object","required":["on"],
			  "properties":{"on":{"type":"boolean"},"labels":{"type":"object","additionalProperties":{"type":"string"}}},
			  "if":{"properties":{"on":{"const":true}}},
			  "then":{"properties":{"labels":{"minProperties":1}}}}}}`,
			admit:  []m{{"o": m{"on": true, "labels": m{"a": "b"}}}, {"o": m{"on": false, "labels": m{}}}},
			refuse: []m{{"o": m{"on": true, "labels": m{}}}},
		},
		{
			name: "a length bound in the condition",
			schema: `{"type":"object","properties":{"o":{"type":"object",
			  "properties":{"name":{"type":"string"},"owner":{"type":"string"}},
			  "if":{"required":["name"],"properties":{"name":{"minLength":1}}},
			  "then":{"required":["owner"]}}}}`,
			admit:  []m{{"o": m{"name": "a", "owner": "x"}}, {"o": m{"name": ""}}, {"o": m{}}},
			refuse: []m{{"o": m{"name": "a"}}},
		},
		{
			// A length bound applies only to strings (JSON Schema), so this branch checks nothing:
			// no rule, and nothing reported dropped.
			name: "a bound that doesn't apply to the declared type checks nothing",
			schema: `{"type":"object","properties":{"o":{"type":"object","required":["on"],
			  "properties":{"on":{"type":"boolean"},"n":{"type":"integer"}},
			  "if":{"properties":{"on":{"const":true}}},
			  "then":{"properties":{"n":{"minLength":3}}}}}}`,
			admit:  []m{{"o": m{"on": true, "n": 1}}},
			refuse: []m{{"o": m{"on": "yes"}}},
		},
		{
			// The same in the condition: it holds for every object, so then always applies.
			name: "a condition that checks nothing applies then to every object",
			schema: `{"type":"object","properties":{"o":{"type":"object",
			  "properties":{"n":{"type":"integer"},"owner":{"type":"string"}},
			  "if":{"properties":{"n":{"minLength":3}}},
			  "then":{"required":["owner"]}}}}`,
			admit:  []m{{"o": m{"owner": "x"}}, {"o": m{"n": 1, "owner": "x"}}},
			refuse: []m{{"o": m{}}, {"o": m{"n": 1}}},
		},
		{
			// In a branch the JSON Schema form holds where the property is absent, which the
			// apiserver's check of service's materialized {} default relies on: mode is absent, so
			// the condition holds, and x, absent from the literal, must not fail it.
			name: "a defaulted property in then holds where it is absent",
			schema: `{"type":"object","properties":{"service":{"type":"object",
			  "properties":{"mode":{"type":"string"},"x":{"type":"string","default":"abc"}},
			  "if":{"properties":{"mode":{"const":"a"}}},
			  "then":{"properties":{"x":{"minLength":2}}}}}}`,
			admit:  []m{{"service": m{}}, {"service": m{"mode": "a", "x": "bc"}}, {"service": m{"mode": "b", "x": "c"}}},
			refuse: []m{{"service": m{"mode": "a", "x": "b"}}},
		},
	})
}

// What a branch can't express is reported, and the rest of it still enforced; a condition that
// can't be expressed drops the whole if/then/else, reported, because a partial condition would
// apply the branch where the schema doesn't.
func TestConditionalConstraintsNotExpressibleAreReported(t *testing.T) {
	sa := func(v m) m { return m{"serviceAccount": v} }
	runCondCases(t, []condCase{
		{
			name: "pattern in then: dropped and reported, required still enforced",
			schema: `{"type":"object","properties":{"serviceAccount":{"type":"object",
			  "properties":{"create":{"type":"boolean"},"name":{"type":"string"}},
			  "if":{"properties":{"create":{"const":false}}},
			  "then":{"required":["name"],"properties":{"name":{"pattern":"^a"}}}}}}`,
			dropped: 1,
			admit:   []m{sa(m{"create": false, "name": "b"}), sa(m{"create": true})},
			refuse:  []m{sa(m{"create": false})},
		},
		{
			name: "a type in then that differs from the declared one: dropped and reported",
			schema: `{"type":"object","properties":{"o":{"type":"object","required":["on"],
			  "properties":{"on":{"type":"boolean"},"n":{"type":"string"}},
			  "if":{"properties":{"on":{"const":true}}},
			  "then":{"required":["n"],"properties":{"n":{"type":"integer"}}}}}}`,
			dropped: 1,
			admit:   []m{{"o": m{"on": true, "n": "x"}}},
			refuse:  []m{{"o": m{"on": true}}},
		},
		{
			name: "minProperties on an object with declared properties: dropped and reported",
			schema: `{"type":"object","properties":{"o":{"type":"object","required":["on"],
			  "properties":{"on":{"type":"boolean"},"c":{"type":"object","properties":{"k":{"type":"string"}}}},
			  "if":{"properties":{"on":{"const":true}}},
			  "then":{"required":["c"],"properties":{"c":{"minProperties":1}}}}}}`,
			dropped: 1,
			admit:   []m{{"o": m{"on": true, "c": m{}}}},
			refuse:  []m{{"o": m{"on": true}}},
		},
		{
			name: "pattern in the condition: the whole if/then/else dropped and reported",
			schema: `{"type":"object","properties":{"o":{"type":"object",
			  "properties":{"name":{"type":"string"},"owner":{"type":"string"}},
			  "if":{"properties":{"name":{"pattern":"^a"}}},
			  "then":{"required":["owner"]}}}}`,
			dropped: 1,
			admit:   []m{{"o": m{"name": "a"}}},
			refuse:  []m{{"o": m{"name": 1}}},
		},
	})

	// The warning names the property and the keyword, so the drop can be found.
	_, warns := generateCond(t, `{"type":"object","properties":{"serviceAccount":{"type":"object",
	  "properties":{"create":{"type":"boolean"},"name":{"type":"string"}},
	  "if":{"properties":{"create":{"const":false}}},
	  "then":{"required":["name"],"properties":{"name":{"pattern":"^a","format":"hostname"}}}}}}`)
	if len(warns) != 1 || !strings.Contains(warns[0].Message, `property "name"`) ||
		!strings.Contains(warns[0].Message, "format, pattern") || warns[0].Path != "spec.serviceAccount" {
		t.Errorf("warnings = %v, want one naming property \"name\", format and pattern, at spec.serviceAccount", warns)
	}
}

// A property the CRD defaults is never absent when the apiserver validates an object, so in a
// condition it counts as present. The one place it is absent is the apiserver's check of a default
// literal: here service's materialized default {} would otherwise match `type: NodePort` by type's
// absence and require nodePort of the CRD's own default, refusing the CRD (measured on the
// portal-service-desk form-options chart before this rule).
func TestConditionalDefaultedPropertyCountsAsPresent(t *testing.T) {
	const schema = `{"type":"object","properties":{"service":{"type":"object",
	  "properties":{"type":{"type":"string","enum":["ClusterIP","NodePort"],"default":"ClusterIP"},
	    "nodePort":{"type":"integer","minimum":30000,"maximum":32767}},
	  "if":{"properties":{"type":{"const":"NodePort"}}},
	  "then":{"required":["nodePort"]}}}}`
	crd, warns := generateCond(t, schema)
	for _, w := range warns {
		if w.Kind == KindConstraintDropped {
			t.Fatalf("a constraint was dropped: %v", w)
		}
	}
	if def := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["service"].Default; def == nil || string(def.Raw) != "{}" {
		t.Fatalf("control: service carries no materialized {} default (%v), so this checks nothing", def)
	}
	for _, ok := range []m{{"service": m{}}, {}, {"service": m{"type": "NodePort", "nodePort": 30001}}} {
		if errs := apiserverAdmits(t, crd, ok); len(errs) != 0 {
			t.Errorf("refused %v: %v", ok, errs)
		}
	}
	if errs := apiserverAdmits(t, crd, m{"service": m{"type": "NodePort"}}); len(errs) == 0 {
		t.Error("admitted type NodePort without a nodePort")
	}
}

// The rules a condition generates, byte for byte where JSON Schema and the old translation agree,
// so CRDs whose conditions name only required or defaulted properties don't change.
func TestConditionalRuleText(t *testing.T) {
	for _, tc := range []struct{ name, schema, rule string }{
		{"required and const in the condition (unchanged)",
			`{"type":"object","properties":{"o":{"type":"object","properties":{"mode":{"type":"string"},"r":{"type":"integer"}},
			  "if":{"required":["mode"],"properties":{"mode":{"const":"prod"}}},"then":{"required":["r"]}}}}`,
			`!(has(self.mode) && has(self.mode) && self.mode == "prod") || (has(self.r))`},
		{"a property the node requires (unchanged)",
			`{"type":"object","properties":{"o":{"type":"object","required":["mode"],"properties":{"mode":{"type":"string"},"r":{"type":"integer"}},
			  "if":{"properties":{"mode":{"enum":["a","b"]}}},"then":{"required":["r"]}}}}`,
			`!(has(self.mode) && (self.mode in ["a", "b"])) || (has(self.r))`},
		{"a property that may be absent",
			`{"type":"object","properties":{"o":{"type":"object","properties":{"create":{"type":"boolean"},"name":{"type":"string"}},
			  "if":{"properties":{"create":{"const":false}}},"then":{"required":["name"],"properties":{"name":{"minLength":1}}}}}}`,
			`!((!has(self.create) || self.create == false)) || (has(self.name) && has(self.name) && size(self.name) >= 1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crd, _ := generateCond(t, tc.schema)
			vs := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["o"].XValidations
			if len(vs) != 1 || vs[0].Rule != tc.rule {
				t.Errorf("rules = %v\nwant %s", vs, tc.rule)
			}
		})
	}
}
