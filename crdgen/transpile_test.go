package crdgen

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

const defaultStatusSchema = `{"type":"object","x-kubernetes-preserve-unknown-fields":true}`

func genDirectVcluster(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	schemaBytes, err := os.ReadFile(filepath.Join("testdata", "direct", "vcluster-0.36.schema.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out, err := Generate(Options{
		Group:        "composition.krateo.io",
		Version:      "0.36.0",
		Kind:         "Vcluster",
		SpecSchema:   schemaBytes,
		StatusSchema: []byte(defaultStatusSchema),
	})
	if err != nil {
		// A non-nil error here means the built-in apiserver validation gate rejected the CRD.
		t.Fatalf("Generate (direct) failed: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatalf("unmarshal generated CRD: %v", err)
	}
	return &crd
}

func specProps(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) map[string]apiextensionsv1.JSONSchemaProps {
	t.Helper()
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec, ok := root.Properties["spec"]
	if !ok {
		t.Fatal("root schema has no spec")
	}
	return spec.Properties
}

// propKeys returns the sorted property keys at a dotted path under spec, or nil if the path
// is absent / not an object.
func propKeys(props map[string]apiextensionsv1.JSONSchemaProps, path ...string) []string {
	cur := props
	for i, seg := range path {
		p, ok := cur[seg]
		if !ok {
			return nil
		}
		if i == len(path)-1 {
			out := make([]string, 0, len(p.Properties))
			for k := range p.Properties {
				out = append(out, k)
			}
			return out
		}
		cur = p.Properties
	}
	return nil
}

func has(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// TestDirectTranspiler_VclusterFidelity asserts the paths the old Go-struct/controller-gen path
// corrupted are now faithful.
func TestDirectTranspiler_VclusterFidelity(t *testing.T) {
	crd := genDirectVcluster(t)
	props := specProps(t, crd)

	// #1: experimental.deploy.vcluster must be {helm, manifests, manifestsTemplate},
	// NOT the invented root-Kind shape {apiVersion, kind, metadata, spec, status}.
	vc := propKeys(props, "experimental", "deploy", "vcluster")
	if len(vc) == 0 {
		t.Fatal("experimental.deploy.vcluster has no properties (dropped)")
	}
	for _, want := range []string{"helm", "manifests"} {
		if !has(vc, want) {
			t.Errorf("experimental.deploy.vcluster missing %q; got %v", want, vc)
		}
	}
	for _, bad := range []string{"apiVersion", "metadata", "status"} {
		if has(vc, bad) {
			t.Errorf("experimental.deploy.vcluster has foreign root-Kind prop %q (collision bug); got %v", bad, vc)
		}
	}

	// #2: controlPlane.statefulSet.resources must be present.
	if ss := propKeys(props, "controlPlane", "statefulSet"); !has(ss, "resources") {
		t.Errorf("controlPlane.statefulSet.resources dropped; statefulSet props=%v", ss)
	}

	// #2: sync.fromHost.nodes must be present.
	if fh := propKeys(props, "sync", "fromHost"); !has(fh, "nodes") {
		t.Errorf("sync.fromHost.nodes dropped; fromHost props=%v", fh)
	}
}

// TestDirectTranspiler_NoPrune proves fidelity with the apiserver's OWN pruner: a CR carrying the
// previously-lost fields must survive Prune untouched.
func TestDirectTranspiler_NoPrune(t *testing.T) {
	crd := genDirectVcluster(t)

	scheme := runtime.NewScheme()
	apiextinstall.Install(scheme)
	var internalSchema apiextensions.JSONSchemaProps
	if err := scheme.Convert(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internalSchema, nil); err != nil {
		t.Fatalf("convert schema to internal: %v", err)
	}
	structural, err := structuralschema.NewStructural(&internalSchema)
	if err != nil {
		t.Fatalf("NewStructural: %v", err)
	}

	cr := map[string]interface{}{
		"apiVersion": "composition.krateo.io/v0-36-0",
		"kind":       "Vcluster",
		"metadata":   map[string]interface{}{"name": "t"},
		"spec": map[string]interface{}{
			"controlPlane": map[string]interface{}{
				"statefulSet": map[string]interface{}{
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{"cpu": "200m", "memory": "256Mi"},
					},
				},
			},
			"sync": map[string]interface{}{
				"fromHost": map[string]interface{}{
					"nodes": map[string]interface{}{"enabled": true},
				},
			},
			"experimental": map[string]interface{}{
				"deploy": map[string]interface{}{
					"vcluster": map[string]interface{}{
						"manifests": "apiVersion: v1\nkind: Namespace\n",
						"helm": []interface{}{
							map[string]interface{}{
								"chart":   map[string]interface{}{"name": "installer", "repo": "oci://ghcr.io/x", "version": "0.2.327"},
								"release": map[string]interface{}{"name": "installer", "namespace": "krateo-system"},
								"values":  "installDefinitions: true\n",
							},
						},
					},
				},
			},
		},
	}

	before := deepCopyJSON(t, cr)
	pruning.Prune(cr, structural, true)
	if !reflect.DeepEqual(before, cr) {
		t.Errorf("Prune removed content — schema does not carry the CR faithfully.\nbefore=%v\nafter =%v", before, cr)
	}
}

// TestDirectTranspiler_Deterministic asserts byte-identical output across runs.
func TestDirectTranspiler_Deterministic(t *testing.T) {
	schemaBytes, err := os.ReadFile(filepath.Join("testdata", "direct", "vcluster-0.36.schema.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	opts := Options{Group: "composition.krateo.io", Version: "0.36.0", Kind: "Vcluster",
		SpecSchema: schemaBytes, StatusSchema: []byte(defaultStatusSchema)}
	a, err := Generate(opts)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	b, err := Generate(opts)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("non-deterministic output: %d vs %d bytes", len(a), len(b))
	}
}

// TestDirectTranspiler_NestedDefs proves a $ref to a $def nested INSIDE another $def
// (#/$defs/Outer/$defs/Inner) resolves — not degraded to an open object.
func TestDirectTranspiler_NestedDefs(t *testing.T) {
	const src = `{
	  "type": "object",
	  "properties": { "outer": { "$ref": "#/$defs/Outer" } },
	  "$defs": {
	    "Outer": {
	      "type": "object",
	      "properties": { "inner": { "$ref": "#/$defs/Outer/$defs/Inner" } },
	      "$defs": {
	        "Inner": { "type": "object", "properties": { "magic": { "type": "string" } } }
	      }
	    }
	  }
	}`
	out, err := Generate(Options{
		Group: "test.krateo.io", Version: "v1", Kind: "Nested",
		SpecSchema: []byte(src), StatusSchema: []byte(defaultStatusSchema),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	props := specProps(t, &crd)
	outer, ok := props["outer"]
	if !ok {
		t.Fatal("spec.outer missing")
	}
	inner, ok := outer.Properties["inner"]
	if !ok {
		t.Fatalf("spec.outer.inner missing; outer props=%v", keysOf(outer.Properties))
	}
	magic, ok := inner.Properties["magic"]
	if !ok || magic.Type != "string" {
		t.Errorf("nested $def #/$defs/Outer/$defs/Inner not resolved; inner props=%v", keysOf(inner.Properties))
	}
}

// TestDirectTranspiler_2020_12 exercises const, dependentRequired/if-then/not via CEL, OAS-3.0
// nullable, and x-kubernetes-* passthrough. Generation succeeding means the apiserver's own
// validation gate accepted the CRD AND compiled every generated CEL rule.
func TestDirectTranspiler_2020_12(t *testing.T) {
	const src = `{
	  "type": "object",
	  "properties": {
	    "mode":     { "type": "string", "const": "prod" },
	    "replicas": { "type": "integer", "nullable": true },
	    "port":     { "x-kubernetes-int-or-string": true },
	    "tls":      { "type": "boolean" },
	    "cert":     { "type": "string" },
	    "key":      { "type": "string" },
	    "region":   { "type": "string", "not": { "enum": ["banned"] } },
	    "list": {
	      "type": "array",
	      "x-kubernetes-list-type": "map",
	      "x-kubernetes-list-map-keys": ["name"],
	      "items": { "type": "object", "required": ["name"], "properties": { "name": { "type": "string" } } }
	    },
	    "custom": {
	      "type": "object",
	      "properties": { "a": { "type": "integer" } },
	      "x-kubernetes-validations": [ { "rule": "self.a > 0", "message": "a must be positive" } ]
	    }
	  },
	  "dependentRequired": { "tls": ["cert", "key"] },
	  "if":   { "properties": { "mode": { "const": "prod" } }, "required": ["mode"] },
	  "then": { "required": ["replicas"] }
	}`
	out, err := Generate(Options{
		Group: "test.krateo.io", Version: "v1", Kind: "Cov",
		SpecSchema: []byte(src), StatusSchema: []byte(defaultStatusSchema),
	})
	if err != nil {
		t.Fatalf("generate (gate rejected — likely bad generated CEL): %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := root.Properties["spec"]
	p := spec.Properties

	if got := p["mode"].Enum; len(got) != 1 || string(got[0].Raw) != `"prod"` {
		t.Errorf("const not mapped to single-value enum; mode.enum=%v", got)
	}
	if !p["replicas"].Nullable {
		t.Errorf("nullable:true not carried on replicas")
	}
	if !p["port"].XIntOrString {
		t.Errorf("x-kubernetes-int-or-string not carried on port")
	}
	if p["list"].XListType == nil || *p["list"].XListType != "map" || len(p["list"].XListMapKeys) != 1 {
		t.Errorf("x-kubernetes-list-type/list-map-keys not carried; list=%+v", p["list"])
	}
	if len(p["custom"].XValidations) != 1 || p["custom"].XValidations[0].Rule != "self.a > 0" {
		t.Errorf("passthrough x-kubernetes-validations lost; custom.validations=%v", p["custom"].XValidations)
	}
	// `not` on a scalar is emitted NATIVELY (no CEL): see TestNotEnumIsNativeAndEnforced.
	if r := p["region"]; r.Not == nil || len(r.Not.Enum) != 1 || string(r.Not.Enum[0].Raw) != `"banned"` || len(r.XValidations) != 0 {
		t.Errorf("not enum on region must be a native not:{enum:[banned]} with no CEL rule; got not=%v validations=%v", r.Not, r.XValidations)
	}
	// dependentRequired + if/then produce CEL rules on the spec object.
	if len(spec.XValidations) < 2 {
		t.Errorf("expected dependentRequired + if/then CEL rules on spec; got %d: %v", len(spec.XValidations), spec.XValidations)
	}
}

func keysOf(m map[string]apiextensionsv1.JSONSchemaProps) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestDirectTranspiler_Corpus asserts the direct path produces an apiserver-accepted CRD for every
// schema in testdata (breadth), via the built-in validation gate inside Generate.
func TestDirectTranspiler_Corpus(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".schema.json") {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if _, err := Generate(Options{
				Group: "test.krateo.io", Version: "v0.0.0", Kind: "Hello",
				SpecSchema: b, StatusSchema: []byte(defaultStatusSchema),
			}); err != nil {
				t.Errorf("direct Generate rejected by apiserver validation: %v", err)
			}
		})
		n++
	}
	if n == 0 {
		t.Skip("no testdata schemas")
	}
}

func deepCopyJSON(t *testing.T, v map[string]interface{}) map[string]interface{} {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
