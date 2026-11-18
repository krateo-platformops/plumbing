//go:build envtest

package valuesschema

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/crdgen"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// D4's defaults reach a composition that OMITS the whole parent map, through crdgen, against a
// real apiserver (authn's review request). The inferred schema puts a default on the leaf
// (image.tag) and none on the parent (image): crdgen's materializeDefaultedParents stamps the parent
// `default: {}`, and that is only safe because inferred schemas emit no `required`
// (TestInfer_NeverEmitsRequired). So creating a composition with no `image` must store
// image.tag = "1.27", and crdgen must say it stamped `image`.
//
// CONTROL: the same CRD with crdgen's parent stamping switched off (Options.NoParentDefaults) must
// store NO image, which proves the leaf default alone does not reach an omitted map and that the
// first arm measures the stamp rather than something else.
func TestEnvtest_InferredLeafDefaultReachesAnOmittedParent(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	r, err := Infer(os.DirFS("testdata"), "basic")
	if err != nil {
		t.Fatal(err)
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	ext := apiextclient.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx := context.Background()

	for _, tc := range []struct {
		kind      string
		noParents bool
		wantTag   any // nil: image absent
	}{
		{"Inferred", false, "1.27"},
		{"InferredNoStamp", true, nil},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			out, warns, err := crdgen.GenerateWithDiagnostics(crdgen.Options{
				Group: "composition.krateo.io", Version: "v1", Kind: tc.kind, SpecSchema: r.Schema, NoParentDefaults: tc.noParents,
			})
			if err != nil {
				t.Fatalf("crdgen: %v", err)
			}
			stamped := false
			for _, w := range warns {
				if w.Kind == crdgen.KindDefaultContradictsSchema {
					t.Errorf("crdgen found a default that contradicts its inferred node: %+v", w)
				}
				if w.Kind == crdgen.KindParentDefaulted && strings.HasSuffix(w.Path, ".image") {
					stamped = true
				}
			}
			if stamped == tc.noParents {
				t.Fatalf("KindParentDefaulted on image = %v with NoParentDefaults=%v (warnings %+v)", stamped, tc.noParents, warns)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(out, &crd); err != nil {
				t.Fatal(err)
			}
			if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
				t.Fatalf("create CRD: %v", err)
			}
			gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: crd.Spec.Names.Plural}
			res := dyn.Resource(gvr).Namespace("default")
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "composition.krateo.io/v1", "kind": tc.kind,
				"metadata": map[string]any{"name": "omits-image", "namespace": "default"},
				"spec":     map[string]any{"replicaCount": 2}, // no `image` at all
			}}
			var created *unstructured.Unstructured
			deadline := time.Now().Add(30 * time.Second)
			for {
				created, err = res.Create(ctx, obj, metav1.CreateOptions{})
				if err == nil || time.Now().After(deadline) {
					break
				}
				time.Sleep(200 * time.Millisecond) // the CRD is not served yet
			}
			if err != nil {
				t.Fatalf("create the composition: %v", err)
			}
			stored, err := res.Get(ctx, created.GetName(), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			tag, found, _ := unstructured.NestedFieldNoCopy(stored.Object, "spec", "image", "tag")
			if tc.wantTag == nil {
				if _, imageFound, _ := unstructured.NestedFieldNoCopy(stored.Object, "spec", "image"); imageFound {
					t.Errorf("control: with no parent stamp the omitted image was still materialised: %v", stored.Object["spec"])
				}
				return
			}
			if !found || tag != tc.wantTag {
				t.Errorf("stored spec.image.tag = %v (found %v), want %v: the leaf default did not reach the omitted parent", tag, found, tc.wantTag)
			}
		})
	}
}

// authn's review of plumbing#215, (2), as Diego decided it, and its re-review: svc's own schema
// REQUIRES foo (a string), labels (an additionalProperties map) and tls (an object with its own
// required mode, plumbing#45's shape), and svc's values.yaml sets all three. helm accepts a release
// that omits any of them (it validates the coalesced values), so the CRD must too: each omitted key
// is filled from its overlaid default, tls without the undeclared key values.yaml also carries.
//
// The overlaid CRD must be ACCEPTED by the apiserver at all (the #45 regression: a default that
// fails its own schema gets the whole CRD rejected), and crdgen must find no contradicting default.
//
// CONTROL: the same subchart schema embedded verbatim refuses each omission with `Required value`.
//
// svc OMITTED is accepted by both: `required` applies to a present object only, and crdgen does not
// stamp a parent that declares `required` (#45; authn: crdgen stays as it is). Asserted, so a change
// to either rule shows up here.
func TestEnvtest_OverlaidDefaultSatisfiesASubchartRequired(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	r, err := Infer(os.DirFS("testdata"), "required")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/required/charts/svc/values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	verbatim := []byte(`{"type":"object","properties":{"replicas":{"x-kubernetes-int-or-string":true,"default":1},"svc":` + string(raw) + `}}`)

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest apiserver: %v", err)
	}
	defer func() { _ = env.Stop() }()
	ext := apiextclient.NewForConfigOrDie(cfg)
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx := context.Background()

	full := map[string]any{"foo": "given", "labels": map[string]any{"team": "given"}, "tls": map[string]any{"mode": "permissive"}}
	without := func(keys ...string) map[string]any {
		out := map[string]any{"port": int64(9090)}
		for k, v := range full {
			if !slices.Contains(keys, k) {
				out[k] = v
			}
		}
		return map[string]any{"svc": out}
	}
	for _, tc := range []struct {
		kind    string
		schema  []byte
		refused bool // every omission below
	}{
		{"Overlaid", r.Schema, false},
		{"Verbatim", verbatim, true},
	} {
		out, warns, err := crdgen.GenerateWithDiagnostics(crdgen.Options{Group: "composition.krateo.io", Version: "v1", Kind: tc.kind, SpecSchema: tc.schema})
		if err != nil {
			t.Fatalf("%s: crdgen: %v", tc.kind, err)
		}
		for _, w := range warns {
			if w.Kind == crdgen.KindDefaultContradictsSchema {
				t.Errorf("%s: crdgen found a default that contradicts its schema: %+v", tc.kind, w)
			}
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(out, &crd); err != nil {
			t.Fatal(err)
		}
		if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crd, metav1.CreateOptions{}); err != nil {
			t.Fatalf("%s: the apiserver rejected the CRD (plumbing#45's failure): %v", tc.kind, err)
		}
		res := dyn.Resource(schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1", Resource: crd.Spec.Names.Plural}).Namespace("default")
		for _, spec := range []struct {
			name string
			spec map[string]any
		}{
			{"without-foo", without("foo")},
			{"without-labels", without("labels")},
			{"without-tls", without("tls")},
			{"without-all", without("foo", "labels", "tls")},
			{"svc-omitted", map[string]any{"replicas": int64(2)}},
		} {
			t.Run(tc.kind+"/"+spec.name, func(t *testing.T) {
				obj := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "composition.krateo.io/v1", "kind": tc.kind,
					"metadata": map[string]any{"name": spec.name, "namespace": "default"},
					"spec":     spec.spec,
				}}
				var err error
				deadline := time.Now().Add(30 * time.Second)
				for {
					_, err = res.Create(ctx, obj, metav1.CreateOptions{})
					if err == nil || !strings.Contains(err.Error(), "the server could not find the requested resource") || time.Now().After(deadline) {
						break
					}
					time.Sleep(200 * time.Millisecond) // the CRD is not served yet
				}
				if spec.name == "svc-omitted" {
					if err != nil {
						t.Fatalf("create = %v: an omitted svc is accepted (required applies to a present object only)", err)
					}
					stored, gerr := res.Get(ctx, spec.name, metav1.GetOptions{})
					if gerr != nil {
						t.Fatal(gerr)
					}
					if _, found, _ := unstructured.NestedFieldNoCopy(stored.Object, "spec", "svc"); found {
						t.Errorf("svc was materialised: crdgen now stamps a parent that declares required; update this test")
					}
					return
				}
				if tc.refused {
					if err == nil || !strings.Contains(err.Error(), "Required value") {
						t.Fatalf("control: create = %v, want a `Required value` refusal from the verbatim schema", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("create: %v (helm accepts this release: svc's values.yaml supplies the key)", err)
				}
				stored, err := res.Get(ctx, spec.name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				svc, _, _ := unstructured.NestedMap(stored.Object, "spec", "svc")
				want := map[string]any{"foo": "given", "labels": map[string]any{"team": "given"}, "tls": map[string]any{"mode": "permissive"}}
				fromValues := map[string]any{"foo": "bar", "labels": map[string]any{"team": "core"}, "tls": map[string]any{"mode": "strict"}}
				for _, k := range []string{"foo", "labels", "tls"} {
					if _, given := spec.spec["svc"].(map[string]any)[k]; !given {
						want[k] = fromValues[k]
					}
					if !reflect.DeepEqual(svc[k], want[k]) {
						t.Errorf("stored spec.svc.%s = %v, want %v", k, svc[k], want[k])
					}
				}
			})
		}
	}
}
