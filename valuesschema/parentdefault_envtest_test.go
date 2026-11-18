package valuesschema

import (
	"context"
	"os"
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
