//go:build envtest

package helm

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/kube"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
)

// core-provider#697: Reconcile hands helm's three-way update only the objects whose own patch is
// non-empty. Every case runs on a real apiserver, with every request counted.

// skipChart: a Deployment, a ConfigMap and a CR (a *.krateo.io kind whose trace stamps are fresh on
// every render, as the post-renderer's are), and a ConfigMap "extra". A release's switch ConfigMap
// (<release>-sw), once it exists, makes the render drop the field cm.data.keep and the child "extra":
// a chart that dropped a field and a child without any change to values or chart version, which is
// what routes a pass through the merge rather than through helm upgrade.
func skipChart() map[string]string {
	const sw = `(lookup "v1" "ConfigMap" "default" (printf "%s-sw" .Release.Name))`
	const sw2 = `(lookup "v1" "ConfigMap" "default" (printf "%s-sw2" .Release.Name))`
	return map[string]string{
		"Chart.yaml": chartYAML,
		"templates/dep.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-dep
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels: {app: {{ .Release.Name }}}
  template:
    metadata:
      labels: {app: {{ .Release.Name }}}
    spec:
      containers:
        - name: c
          image: busybox:1.36
`,
		"templates/cm.yaml": cmDoc("cm", "  a: \"1\"\n{{- if not "+sw+" }}\n  keep: me\n{{- end }}\n"),
		"templates/widget.yaml": `apiVersion: skip.krateo.io/v1
kind: Widget
metadata:
  name: {{ .Release.Name }}-w
  namespace: default
  annotations:
    krateo.io/traceparent: 00-{{ randAlphaNum 32 | lower }}-{{ randAlphaNum 16 | lower }}-01
    krateo.io/tracestate: vendor={{ randAlphaNum 8 }}
spec:
  size: s
`,
		"templates/extra.yaml": "{{- if not " + sw2 + " }}\n" + cmDoc("extra", "  e: \"1\"\n") + "{{- end }}\n",
	}
}

type skipEnv struct {
	*wnEnv
	dyn dynamic.Interface
	url string
}

var widgetGVR = schema.GroupVersionResource{Group: "skip.krateo.io", Version: "v1", Resource: "widgets"}

func newSkipEnv(t *testing.T) *skipEnv {
	t.Helper()
	e := &skipEnv{wnEnv: newWNEnv(t)}
	ctx := context.Background()
	ext, err := apiextclient.NewForConfig(e.hc.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.skip.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "skip.krateo.io", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": {Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{"size": {Type: "string"}}}}}}}},
		},
	}
	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEstablished(t, ext, crd.Name)
	e.dyn, err = dynamic.NewForConfig(e.hc.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	e.url = serveFiles(t, skipChart())
	return e
}

// install makes release rel; its objects are <rel>-dep, -cm, -w, and -extra.
func (e *skipEnv) install(t *testing.T, rel string) {
	t.Helper()
	if _, err := e.hc.Install(context.Background(), rel, e.url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
		t.Fatalf("install %s: %v", rel, err)
	}
	for _, p := range []string{"-dep", "-cm", "-w", "-extra"} {
		if e.objectPath(rel+p) == "" {
			t.Fatalf("control: %s%s missing", rel, p)
		}
	}
}

// objectPath is the REST path of the object named name, in default.
func (e *skipEnv) objectPath(name string) string {
	switch {
	case strings.HasSuffix(name, "-dep"):
		return "/apis/apps/v1/namespaces/default/deployments/" + name
	case strings.HasSuffix(name, "-w"):
		return "/apis/skip.krateo.io/v1/namespaces/default/widgets/" + name
	default:
		return "/api/v1/namespaces/default/configmaps/" + name
	}
}

// pass is one Reconcile and what it sent.
type pass struct {
	res  *helmconfig.ReconcileResult
	err  error
	reqs []recordedReq
}

func (e *skipEnv) reconcile(rel string) pass {
	e.rec.start()
	res, err := e.hc.Reconcile(context.Background(), rel, e.url, &helmconfig.UpgradeConfig{MaxHistory: 3})
	return pass{res, err, e.rec.stop()}
}

// count is how many requests of method went to exactly path.
func (p pass) count(method, path string) int {
	n := 0
	for _, r := range p.reqs {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func (p pass) writes() (out []string) {
	for _, r := range p.reqs {
		if !isReadMethod(r.method) {
			out = append(out, r.method+" "+r.path)
		}
	}
	return out
}

// overhead is every GET that is not a read of one of the release's objects (own: their paths), with
// the release's name collapsed, so it can be named and compared.
func (p pass) overhead(rel string, own map[string]bool) map[string]int {
	out := map[string]int{}
	for _, r := range p.reqs {
		if r.method == http.MethodGet && !own[r.path] {
			out["GET "+strings.ReplaceAll(r.path, rel, "<rel>")]++
		}
	}
	return out
}

func sortedKeys(m map[string]int) []string {
	var out []string
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s x%d", k, v))
	}
	sort.Strings(out)
	return out
}

func (e *skipEnv) objs(rel string, suffixes ...string) []string {
	var out []string
	for _, s := range suffixes {
		out = append(out, rel+s)
	}
	return out
}

// The steady state: one GET per object, and no write.
func TestEnvtest_SkipUndrifted_SteadyStateReadsEachObjectOnce(t *testing.T) {
	e := newSkipEnv(t)
	rel := "steady"
	e.install(t, rel)
	names := e.objs(rel, "-dep", "-cm", "-w", "-extra")
	e.reconcile(rel) // the first pass after an install can still settle server-owned fields

	p := e.reconcile(rel)
	if p.err != nil || p.res == nil {
		t.Fatalf("reconcile: %v", p.err)
	}
	total := 0
	for _, n := range names {
		got := p.count(http.MethodGet, e.objectPath(n))
		t.Logf("GET %-40s %d", e.objectPath(n), got)
		if got != 1 {
			t.Errorf("GET of %s = %d, want exactly 1", n, got)
		}
		total += got
	}
	if w := p.writes(); len(w) > 0 {
		t.Errorf("a steady-state pass wrote: %v", w)
	}
	t.Logf("object GETs: %d (%d objects); PATCH: %d", total, len(names), 0)
	own := map[string]bool{}
	for _, n := range names {
		own[e.objectPath(n)] = true
	}
	t.Logf("fixed overhead (named): %v", sortedKeys(p.overhead(rel, own)))

	// ReconcileResult keeps its meaning at steady state.
	r := p.res
	if r.Changed || r.AppliedPartially || r.Created+r.Deleted+r.PatchedUpdated != 0 || len(r.ChangedObjects) != 0 || len(r.Objects) != 0 || !r.ObjectsComplete {
		t.Errorf("steady-state result changed meaning: %+v", r)
	}
	if r.ObjectsNotAttempted != 0 {
		t.Errorf("ObjectsNotAttempted = %d, want 0 (a skipped object is unchanged, not unattempted)", r.ObjectsNotAttempted)
	}
}

// switchOn creates the release's switch ConfigMap (see skipChart): the next render drops cm.data.keep
// and the child "extra".
func (e *skipEnv) switchOn(t *testing.T, name string) {
	t.Helper()
	_, err := e.cs.CoreV1().ConfigMaps("default").Create(context.Background(),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

func (e *skipEnv) cm(t *testing.T, name string) *corev1.ConfigMap {
	t.Helper()
	cm, err := e.cs.CoreV1().ConfigMaps("default").Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return cm
}

func (e *skipEnv) widget(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	w, err := e.dyn.Resource(widgetGVR).Namespace("default").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// patched is the objects the pass's MERGE sent a PATCH to: the requests before the first write to
// release storage. Step 7's real upgrade, which runs when the merge changed something, writes the
// revision and re-stamps the *.krateo.io children, as it always has; it is not the merge.
func patched(p pass) []string {
	var out []string
	for _, r := range p.reqs {
		if !isReadMethod(r.method) && strings.Contains(r.path, "/secrets") {
			break
		}
		if r.method == http.MethodPatch {
			out = append(out, r.path)
		}
	}
	return out
}

func assertNames(t *testing.T, what string, res *helmconfig.ReconcileResult, want ...string) {
	t.Helper()
	var got []string
	for _, n := range res.ChangedObjects {
		got = append(got, n)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: ChangedObjects = %v, want %v", what, got, want)
	}
}

// Drift is healed in the same pass, and only the drifted object is written.
func TestEnvtest_SkipUndrifted_DriftHealedInTheSamePass(t *testing.T) {
	e := newSkipEnv(t)
	ctx := context.Background()

	t.Run("built-in field", func(t *testing.T) {
		rel := "dep"
		e.install(t, rel)
		e.reconcile(rel)
		d, err := e.cs.AppsV1().Deployments("default").Get(ctx, rel+"-dep", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		d.Spec.Replicas = ptrTo(int32(3))
		if _, err := e.cs.AppsV1().Deployments("default").Update(ctx, d, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		p := e.reconcile(rel)
		if p.err != nil {
			t.Fatal(p.err)
		}
		d, _ = e.cs.AppsV1().Deployments("default").Get(ctx, rel+"-dep", metav1.GetOptions{})
		if *d.Spec.Replicas != 1 {
			t.Errorf("replicas = %d, want healed to 1", *d.Spec.Replicas)
		}
		assertNames(t, "deployment", p.res, "apps/v1/Deployment/default/"+rel+"-dep")
		if got := patched(p); len(got) != 1 || got[0] != e.objectPath(rel+"-dep") {
			t.Errorf("PATCHed %v, want only the Deployment", got)
		}
	})

	t.Run("CR field", func(t *testing.T) {
		rel := "cr"
		e.install(t, rel)
		e.reconcile(rel)
		w := e.widget(t, rel+"-w")
		_ = unstructured.SetNestedField(w.Object, "edited", "spec", "size")
		if _, err := e.dyn.Resource(widgetGVR).Namespace("default").Update(ctx, w, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		p := e.reconcile(rel)
		if p.err != nil {
			t.Fatal(p.err)
		}
		if v, _, _ := unstructured.NestedString(e.widget(t, rel+"-w").Object, "spec", "size"); v != "s" {
			t.Errorf("spec.size = %q, want healed to s", v)
		}
		assertNames(t, "CR", p.res, "skip.krateo.io/v1/Widget/default/"+rel+"-w")
		if got := patched(p); len(got) != 1 || got[0] != e.objectPath(rel+"-w") {
			t.Errorf("PATCHed %v, want only the CR", got)
		}
	})

	t.Run("deleted child", func(t *testing.T) {
		rel := "del"
		e.install(t, rel)
		e.reconcile(rel)
		if err := e.cs.CoreV1().ConfigMaps("default").Delete(ctx, rel+"-cm", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		p := e.reconcile(rel)
		if p.err != nil {
			t.Fatal(p.err)
		}
		if e.cm(t, rel+"-cm") == nil {
			t.Fatal("the deleted child was not re-created")
		}
		if p.res.Created != 1 {
			t.Errorf("Created = %d, want 1", p.res.Created)
		}
		if got := patched(p); len(got) != 0 {
			t.Errorf("PATCHed %v, want nothing (a creation is a POST)", got)
		}
	})

	t.Run("only a deletion", func(t *testing.T) {
		rel := "onlydel"
		e.install(t, rel)
		e.reconcile(rel)
		e.switchOn(t, rel+"-sw2")
		p := e.reconcile(rel)
		if p.err != nil {
			t.Fatal(p.err)
		}
		if e.cm(t, rel+"-extra") != nil {
			t.Errorf("the child the chart dropped is still live: the merge was not asked to delete it")
		}
		for _, n := range []string{"-dep", "-cm", "-w"} {
			if !e.objectExists(t, rel+n) {
				t.Errorf("%s%s was deleted", rel, n)
			}
		}
		if p.res.Deleted != 1 || p.res.PatchedUpdated != 0 {
			t.Errorf("Deleted/PatchedUpdated = %d/%d, want 1/0", p.res.Deleted, p.res.PatchedUpdated)
		}
		if got := patched(p); len(got) != 0 {
			t.Errorf("PATCHed %v, want nothing", got)
		}
	})

	t.Run("field the chart dropped, still live", func(t *testing.T) {
		rel := "drop"
		e.install(t, rel)
		e.reconcile(rel)
		if e.cm(t, rel+"-cm").Data["keep"] != "me" {
			t.Fatal("control: the install must have written cm.data.keep")
		}
		e.switchOn(t, rel+"-sw")
		e.switchOn(t, rel+"-sw2")
		p := e.reconcile(rel)
		if p.err != nil {
			t.Fatal(p.err)
		}
		if _, still := e.cm(t, rel+"-cm").Data["keep"]; still {
			t.Errorf("cm.data.keep is still live, the chart dropped it")
		}
		// The same render dropped the child "extra" altogether: a deletion, alongside the objects the
		// merge was not handed. None of those may be deleted for having left the merge.
		if e.cm(t, rel+"-extra") != nil {
			t.Errorf("the dropped child is still live")
		}
		for _, n := range []string{"-dep", "-w"} {
			if !e.objectExists(t, rel+n) {
				t.Errorf("%s%s was deleted: an object left out of the merge must leave both of its lists", rel, n)
			}
		}
		if p.res.Deleted != 1 {
			t.Errorf("Deleted = %d, want 1", p.res.Deleted)
		}
		if got := patched(p); len(got) != 1 || got[0] != e.objectPath(rel+"-cm") {
			t.Errorf("PATCHed %v, want only the ConfigMap that lost a field", got)
		}
	})
}

func (e *skipEnv) objectExists(t *testing.T, name string) bool {
	t.Helper()
	ctx := context.Background()
	var err error
	switch {
	case strings.HasSuffix(name, "-dep"):
		_, err = e.cs.AppsV1().Deployments("default").Get(ctx, name, metav1.GetOptions{})
	case strings.HasSuffix(name, "-w"):
		_, err = e.dyn.Resource(widgetGVR).Namespace("default").Get(ctx, name, metav1.GetOptions{})
	default:
		_, err = e.cs.CoreV1().ConfigMaps("default").Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// A child whose only live difference from the render is its trace stamps is not written.
func TestEnvtest_SkipUndrifted_TraceStampsAreNotDrift(t *testing.T) {
	e := newSkipEnv(t)
	ctx := context.Background()
	rel := "trace"
	e.install(t, rel)
	e.reconcile(rel)

	w := e.widget(t, rel+"-w")
	ann := w.GetAnnotations()
	ann["krateo.io/traceparent"] = "00-11111111111111111111111111111111-2222222222222222-01"
	ann["krateo.io/tracestate"] = "vendor=other"
	w.SetAnnotations(ann)
	if _, err := e.dyn.Resource(widgetGVR).Namespace("default").Update(ctx, w, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	p := e.reconcile(rel)
	if p.err != nil {
		t.Fatal(p.err)
	}
	if wr := p.writes(); len(wr) > 0 {
		t.Errorf("a child whose only difference is its trace stamps was written: %v", wr)
	}
	if p.res.Changed || len(p.res.ChangedObjects) != 0 || len(p.res.Objects) != 0 {
		t.Errorf("result = %+v, want unchanged", p.res)
	}
	if got := e.widget(t, rel+"-w").GetAnnotations()["krateo.io/traceparent"]; got != "00-11111111111111111111111111111111-2222222222222222-01" {
		t.Errorf("the live traceparent was replaced: %q", got)
	}
	if n := p.count(http.MethodGet, e.objectPath(rel+"-w")); n != 1 {
		t.Errorf("GETs of the CR = %d, want 1 (it was skipped)", n)
	}
}

// An object helm's update would patch is exactly an object this decision does not skip: the decision
// is compared with helm's own update on the same inputs, over each kind of live difference.
func TestEnvtest_SkipUndrifted_DecisionAgreesWithHelmsUpdate(t *testing.T) {
	e := newSkipEnv(t)
	ctx := context.Background()
	edits := map[string]func(t *testing.T, rel string){
		"none": func(*testing.T, string) {},
		"deployment replicas": func(t *testing.T, rel string) {
			d, _ := e.cs.AppsV1().Deployments("default").Get(ctx, rel+"-dep", metav1.GetOptions{})
			d.Spec.Replicas = ptrTo(int32(4))
			if _, err := e.cs.AppsV1().Deployments("default").Update(ctx, d, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		},
		"deployment label (live-only, not in the render)": func(t *testing.T, rel string) {
			d, _ := e.cs.AppsV1().Deployments("default").Get(ctx, rel+"-dep", metav1.GetOptions{})
			d.Labels = map[string]string{"added": "live"}
			if _, err := e.cs.AppsV1().Deployments("default").Update(ctx, d, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		},
		"configmap value": func(t *testing.T, rel string) {
			cm := e.cm(t, rel+"-cm")
			cm.Data["a"] = "2"
			if _, err := e.cs.CoreV1().ConfigMaps("default").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		},
		"configmap field the stored manifest has and live lost": func(t *testing.T, rel string) {
			cm := e.cm(t, rel+"-cm")
			delete(cm.Data, "keep")
			if _, err := e.cs.CoreV1().ConfigMaps("default").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		},
		"CR field": func(t *testing.T, rel string) {
			w := e.widget(t, rel+"-w")
			_ = unstructured.SetNestedField(w.Object, "edited", "spec", "size")
			if _, err := e.dyn.Resource(widgetGVR).Namespace("default").Update(ctx, w, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		},
		"CR annotation (live-only)": func(t *testing.T, rel string) {
			w := e.widget(t, rel+"-w")
			ann := w.GetAnnotations()
			ann["other"] = "x"
			w.SetAnnotations(ann)
			if _, err := e.dyn.Resource(widgetGVR).Namespace("default").Update(ctx, w, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		},
	}
	i := 0
	for name, edit := range edits {
		i++
		rel := fmt.Sprintf("agree%d", i)
		t.Run(name, func(t *testing.T) {
			e.install(t, rel)
			e.reconcile(rel)
			edit(t, rel)

			ac, err := e.hc.newActionConfig(ctx, "default", e.hc.restConfig, nil)
			if err != nil {
				t.Fatal(err)
			}
			rls, err := e.hc.GetRelease(ctx, rel, &helmconfig.GetConfig{})
			if err != nil {
				t.Fatal(err)
			}
			kc := ac.KubeClient
			// The target is the stored manifest itself: the same objects, as the render would give them
			// without per-render stamps.
			current, err := kc.Build(strings.NewReader(rls.Manifest), false)
			if err != nil {
				t.Fatal(err)
			}
			target, err := kc.Build(strings.NewReader(rls.Manifest), true)
			if err != nil {
				t.Fatal(err)
			}
			live := map[string]runtime.Object{}
			for _, info := range target {
				o, gerr := resource.NewHelper(info.Client, info.Mapping).Get(info.Namespace, info.Name)
				if gerr != nil {
					t.Fatal(gerr)
				}
				live[infoKey(info)] = o
			}
			skipped := map[string]bool{}
			for _, info := range undriftedTargets(current, target, live, map[string]bool{}, map[string]bool{}) {
				skipped[info.Name] = true
			}

			e.rec.start()
			if _, err := kc.(kube.InterfaceThreeWayMerge).UpdateThreeWayMerge(current, target, false); err != nil {
				t.Fatal(err)
			}
			reqs := e.rec.stop()
			helmPatched := map[string]bool{}
			for _, r := range reqs {
				if r.method == http.MethodPatch {
					helmPatched[r.path[strings.LastIndex(r.path, "/")+1:]] = true
				}
			}
			for _, info := range target {
				if skipped[info.Name] == helmPatched[info.Name] {
					t.Errorf("%s: skipped=%v but helm's update patched=%v", info.Name, skipped[info.Name], helmPatched[info.Name])
				}
			}
			t.Logf("skipped %d of %d; helm patched %v", len(skipped), len(target), helmPatched)
		})
	}
}

// An adoption (a live child the stored manifest lacks, step 4.7) is handed to the merge even when its
// patch is empty: it is not left out as an undrifted object.
func TestEnvtest_SkipUndrifted_AdoptionStillGoesThroughTheMerge(t *testing.T) {
	e := newSkipEnv(t)
	rel := "adopt"
	e.install(t, rel)
	e.reconcile(rel)

	last, err := e.st.Last(rel)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, doc := range strings.Split(last.Manifest, "---\n") {
		if !strings.Contains(doc, "name: "+rel+"-cm\n") {
			kept = append(kept, doc)
		}
	}
	if len(kept) == strings.Count(last.Manifest, "---\n")+1 {
		t.Fatal("control: the ConfigMap's document was not found in the stored manifest")
	}
	last.Manifest = strings.Join(kept, "---\n")
	if err := e.st.Update(last); err != nil {
		t.Fatal(err)
	}

	p := e.reconcile(rel)
	if p.err != nil {
		t.Fatal(p.err)
	}
	t.Logf("GETs: cm=%d dep=%d", p.count(http.MethodGet, e.objectPath(rel+"-cm")), p.count(http.MethodGet, e.objectPath(rel+"-dep")))
	// A child that goes through the merge is read by step 4 and three more times by helm's update.
	if got := p.count(http.MethodGet, e.objectPath(rel+"-cm")); got < 4 {
		t.Errorf("the adopted ConfigMap was read %d time(s): it was left out of the merge", got)
	}
	if got := p.count(http.MethodGet, e.objectPath(rel+"-dep")); got != 1 {
		t.Errorf("the Deployment was read %d times, want 1 (undrifted, skipped)", got)
	}
	if wr := p.writes(); len(wr) > 0 {
		t.Errorf("adopting an undrifted child wrote: %v", wr)
	}
	if p.res.Deleted != 0 || e.cm(t, rel+"-cm") == nil {
		t.Errorf("the adopted child was deleted: %+v", p.res)
	}
}
