//go:build envtest

package helm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// retryEnv is one apiserver shared by the cases; each uses its own release name, and every object name
// starts with it.
type retryEnv struct {
	hc   *client
	kube kubernetes.Interface
	dyn  dynamic.Interface
}

func newRetryEnv(t *testing.T) *retryEnv {
	t.Helper()
	hc := envtestClient(t)
	kube, err := kubernetes.NewForConfig(hc.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(hc.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	return &retryEnv{hc, kube, dyn}
}

func (e *retryEnv) cm(t *testing.T, name string) *corev1.ConfigMap {
	t.Helper()
	cm, err := e.kube.CoreV1().ConfigMaps("default").Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return cm
}

// first is the plain install the retry is for; it must fail.
func (e *retryEnv) first(t *testing.T, rel, url string, vals map[string]any) {
	t.Helper()
	if _, err := e.hc.Install(context.Background(), rel, url, &helmconfig.InstallConfig{Namespace: "default",
		ActionConfig: &helmconfig.ActionConfig{Values: vals}}); err == nil {
		t.Fatal("control: the first install must fail")
	}
}

func (e *retryEnv) retry(rel, url string, vals map[string]any, mut ...func(*helmconfig.InstallConfig)) (*helmconfig.OperationResult, error) {
	cfg := &helmconfig.InstallConfig{Namespace: "default", RetryFailed: true, MaxHistory: 3,
		ActionConfig: &helmconfig.ActionConfig{Values: vals}}
	for _, m := range mut {
		m(cfg)
	}
	return e.hc.Install(context.Background(), rel, url, cfg)
}

func (e *retryEnv) last(t *testing.T, rel string) *helmconfig.Release {
	t.Helper()
	r, err := e.hc.GetRelease(context.Background(), rel, &helmconfig.GetConfig{})
	if err != nil || r == nil {
		t.Fatalf("release %s: %v %v", rel, r, err)
	}
	return r
}

func cmDoc(suffix, data string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-" + suffix + "\n  namespace: default\ndata:\n" + data
}

// hookDoc is a hook that fails while .Values.<flag> is "bad" (its namespace does not exist).
func hookDoc(suffix, events, flag string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-" + suffix +
		"\n  namespace: {{ if eq (.Values." + flag + " | default \"ok\") \"bad\" }}no-such-namespace{{ else }}default{{ end }}\n" +
		"  annotations:\n    helm.sh/hook: " + events + "\n    helm.sh/hook-delete-policy: before-hook-creation\ndata:\n  k: v\n"
}

const unlessEdited = "{{- if not .Values.edited }}\n"

// leftoverChart: while not edited, the ConfigMap has a key more and there is one object more.
func leftoverChart() map[string]string {
	return map[string]string{
		"Chart.yaml":          chartYAML,
		"templates/ok.yaml":   cmDoc("ok", "  k: {{ .Values.k | default \"v1\" | quote }}\n"+unlessEdited+"  gone: x\n{{- end }}\n"),
		"templates/orph.yaml": unlessEdited + cmDoc("orphan", "  a: b\n") + "{{- end }}\n",
		"templates/inst.yaml": "{{- if .Release.IsInstall }}\n" + cmDoc("installonly", "  a: b\n") + "{{- end }}\n",
		"templates/pre.yaml":  hookDoc("pre", "pre-install", "pre"),
		"templates/post.yaml": hookDoc("post", "post-install", "post"),
		"templates/up.yaml":   hookDoc("up", "pre-upgrade,post-upgrade", "never"),
	}
}

func TestEnvtest_RetryFailed(t *testing.T) {
	e := newRetryEnv(t)
	ctx := context.Background()
	url := serveFiles(t, leftoverChart())
	bad, edited := map[string]any{"post": "bad"}, map[string]any{"edited": true, "k": "v2"}

	for _, own := range []bool{false, true} {
		t.Run("edited values leave nothing behind, TakeOwnership="+map[bool]string{true: "true", false: "false"}[own], func(t *testing.T) {
			rel := "left" + map[bool]string{true: "a", false: "b"}[own]
			e.first(t, rel, url, bad)
			ok, inst := e.cm(t, rel+"-ok"), e.cm(t, rel+"-installonly")
			if ok == nil || ok.Data["gone"] != "x" || e.cm(t, rel+"-orphan") == nil || inst == nil {
				t.Fatal("control: the failed install must have created its objects")
			}
			if e.cm(t, rel+"-post") != nil {
				t.Fatal("control: the post-install hook must not have run")
			}
			op, err := e.retry(rel, url, edited, func(c *helmconfig.InstallConfig) { c.TakeOwnership = own })
			if err != nil {
				t.Fatal(err)
			}
			if op.Release.Status != helmconfig.StatusDeployed || op.Release.Revision != 2 {
				t.Fatalf("release = %+v", op.Release)
			}
			after := e.cm(t, rel+"-ok")
			if after.UID != ok.UID || after.Data["k"] != "v2" || len(after.Data) != 1 {
				t.Errorf("ok: same=%v data=%v, want the same object with k: v2 only", after.UID == ok.UID, after.Data)
			}
			if e.cm(t, rel+"-orphan") != nil {
				t.Error("the dropped object is still there")
			}
			if got := e.cm(t, rel+"-installonly"); got == nil || got.UID != inst.UID {
				t.Error("an object rendered only on install must be kept, not deleted or recreated")
			}
			if e.cm(t, rel+"-post") == nil || e.cm(t, rel+"-up") != nil {
				t.Error("the install hooks must run and the upgrade hooks must not")
			}
			verdicts := map[string]helmconfig.ObjectChange{}
			for _, o := range op.Objects {
				verdicts[o.Name] = o.Change
			}
			if verdicts[rel+"-orphan"] != helmconfig.ChangeDeleted {
				t.Errorf("verdicts = %v, want the orphan deleted", verdicts)
			}
			if stored := e.last(t, rel).Manifest; strings.Contains(stored, rel+"-orphan") || stored != op.Release.Manifest {
				t.Errorf("a deployed retry stores its render:\n%s", stored)
			}
			if _, err := e.hc.Uninstall(ctx, rel, &helmconfig.UninstallConfig{}); err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{"ok", "orphan", "installonly"} {
				if e.cm(t, rel+"-"+s) != nil {
					t.Errorf("%s survives the uninstall", s)
				}
			}
		})
	}

	t.Run("an attempt that applied nothing does not hide the one before it", func(t *testing.T) {
		rel := "carry"
		e.first(t, rel, url, bad)
		before := e.last(t, rel).Manifest
		if _, err := e.retry(rel, url, map[string]any{"edited": true, "pre": "bad"}); err == nil {
			t.Fatal("control: the pre-install hook must fail the second attempt")
		}
		if got := e.last(t, rel); got.Status != helmconfig.StatusFailed || got.Revision != 2 || got.Manifest != before {
			t.Fatalf("revision %d %s must store what is applied, the first attempt's manifest:\n%s", got.Revision, got.Status, got.Manifest)
		}
		if e.cm(t, rel+"-ok").Data["gone"] != "x" || e.cm(t, rel+"-orphan") == nil {
			t.Fatal("control: the second attempt must have applied nothing")
		}
		if _, err := e.retry(rel, url, edited); err != nil {
			t.Fatal(err)
		}
		if cm := e.cm(t, rel+"-ok"); len(cm.Data) != 1 || e.cm(t, rel+"-orphan") != nil {
			t.Errorf("left behind: ok=%v orphan=%v", cm.Data, e.cm(t, rel+"-orphan") != nil)
		}
	})

	t.Run("pre-install hooks run before the objects", func(t *testing.T) {
		rel := "order"
		e.first(t, rel, url, map[string]any{"pre": "bad"})
		if _, err := e.retry(rel, url, map[string]any{"pre": "bad"}); err == nil {
			t.Fatal("control: the hook must fail again")
		}
		if e.cm(t, rel+"-ok") != nil {
			t.Fatal("the objects were applied although the pre-install hook failed")
		}
		if _, err := e.retry(rel, url, nil); err != nil {
			t.Fatal(err)
		}
		if e.cm(t, rel+"-ok") == nil || e.cm(t, rel+"-pre") == nil {
			t.Fatal("the retry that can succeed must run the hook and create the objects")
		}
	})

	t.Run("a hook that keeps failing stays failed within MaxHistory", func(t *testing.T) {
		rel := "again"
		e.first(t, rel, url, bad)
		for i := 0; i < 4; i++ {
			if _, err := e.retry(rel, url, bad); err == nil {
				t.Fatal("control: the hook must fail again")
			}
		}
		h, err := e.hc.History(ctx, rel, &helmconfig.HistoryConfig{})
		if err != nil || len(h) != 3 || h[2].Revision != 5 || h[2].Status != helmconfig.StatusFailed {
			t.Fatalf("history = %v, %v; want 3 revisions ending at a failed 5", revisions(h), err)
		}
		if _, err := e.retry(rel, url, edited); err != nil {
			t.Fatal(err)
		}
		if e.cm(t, rel+"-orphan") != nil {
			t.Error("left behind after revision 1 was pruned")
		}
	})

	t.Run("a render that shares no live object with the failed one", func(t *testing.T) {
		rel := "swap"
		u := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/post.yaml": hookDoc("post", "post-install", "post"),
			"templates/a.yaml": unlessEdited + cmDoc("a", "  k: v\n") + "{{- else }}\n" + cmDoc("b", "  k: v\n") + "{{- end }}\n"})
		e.first(t, rel, u, bad)
		if _, err := e.retry(rel, u, edited); err != nil {
			t.Fatal(err)
		}
		if e.cm(t, rel+"-a") != nil || e.cm(t, rel+"-b") == nil {
			t.Errorf("a present=%v, b present=%v", e.cm(t, rel+"-a") != nil, e.cm(t, rel+"-b") != nil)
		}
	})

	t.Run("a render with no objects", func(t *testing.T) {
		rel := "empty"
		u := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/post.yaml": hookDoc("post", "post-install", "post"),
			"templates/a.yaml": unlessEdited + cmDoc("a", "  k: v\n") + "{{- end }}\n"})
		e.first(t, rel, u, bad)
		if e.cm(t, rel+"-a") == nil {
			t.Fatal("control: a must exist")
		}
		if _, err := e.retry(rel, u, edited); err != nil {
			t.Fatal(err)
		}
		if e.cm(t, rel+"-a") != nil {
			t.Error("the dropped object is still there")
		}
	})

	t.Run("what is not this release's, or is kept, is not deleted", func(t *testing.T) {
		rel := "safe"
		u := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/ok.yaml": cmDoc("ok", "  k: v\n"),
			"templates/pre.yaml":  hookDoc("pre", "pre-install", "pre"),
			"templates/post.yaml": hookDoc("post", "post-install", "post"),
			"templates/keep.yaml": unlessEdited + strings.Replace(cmDoc("keep", "  k: v\n"), "  namespace: default\n", "  namespace: default\n  annotations:\n    helm.sh/resource-policy: keep\n", 1) + "{{- end }}\n",
			"templates/own.yaml":  unlessEdited + cmDoc("own", "  k: v\n") + "{{- end }}\n",
			"templates/for.yaml":  unlessEdited + cmDoc("foreign", "  k: v\n") + "{{- end }}\n"})
		e.first(t, rel, u, bad)
		// Somebody else now holds one of the names.
		if err := e.kube.CoreV1().ConfigMaps("default").Delete(ctx, rel+"-foreign", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.kube.CoreV1().ConfigMaps("default").Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: rel + "-foreign"}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.retry(rel, u, edited); err != nil {
			t.Fatal(err)
		}
		if e.cm(t, rel+"-foreign") == nil {
			t.Error("an object that is not this release's was deleted")
		}
		if e.cm(t, rel+"-keep") == nil {
			t.Error("a kept object was deleted")
		}
		if e.cm(t, rel+"-own") != nil {
			t.Error("control: the release's own dropped object must be deleted")
		}
	})

	t.Run("a Secret key dropped from stringData", func(t *testing.T) {
		rel := "secret"
		u := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/post.yaml": hookDoc("post", "post-install", "post"),
			"templates/s.yaml": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: {{ .Release.Name }}-s\n  namespace: default\nstringData:\n  k: v\n" + unlessEdited + "  gone: x\n{{- end }}\n"})
		e.first(t, rel, u, bad)
		if _, err := e.retry(rel, u, edited); err != nil {
			t.Fatal(err)
		}
		s, err := e.kube.CoreV1().Secrets("default").Get(ctx, rel+"-s", metav1.GetOptions{})
		if err != nil || len(s.Data) != 1 || string(s.Data["k"]) != "v" {
			t.Errorf("secret has %d keys, %v; want k only", len(s.Data), err)
		}
	})

	t.Run("a custom resource's changed and dropped fields", func(t *testing.T) {
		const crd = "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: widgets.retry.test\nspec:\n  group: retry.test\n  scope: Namespaced\n  names: {kind: Widget, plural: widgets, singular: widget, listKind: WidgetList}\n  versions:\n  - name: v1\n    served: true\n    storage: true\n    schema:\n      openAPIV3Schema:\n        type: object\n        x-kubernetes-preserve-unknown-fields: true\n"
		u := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "crds/w.yaml": crd, "templates/post.yaml": hookDoc("post", "post-install", "post"),
			"templates/w.yaml": "apiVersion: retry.test/v1\nkind: Widget\nmetadata:\n  name: {{ .Release.Name }}-w\n  namespace: default\nspec:\n  k: {{ .Values.k | default \"v1\" }}\n" + unlessEdited + "  gone: x\n{{- end }}\n"})
		for _, own := range []bool{false, true} {
			rel := "cr" + map[bool]string{true: "a", false: "b"}[own]
			e.first(t, rel, u, bad)
			if _, err := e.retry(rel, u, edited, func(c *helmconfig.InstallConfig) { c.TakeOwnership = own }); err != nil {
				t.Fatal(err)
			}
			w, err := e.dyn.Resource(schema.GroupVersionResource{Group: "retry.test", Version: "v1", Resource: "widgets"}).Namespace("default").Get(ctx, rel+"-w", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			spec, _, _ := unstructured.NestedStringMap(w.Object, "spec")
			if len(spec) != 1 || spec["k"] != "v2" {
				t.Errorf("TakeOwnership=%v: spec = %v, want k: v2 only", own, spec)
			}
		}
	})

	t.Run("a partial apply stores each object as it is", func(t *testing.T) {
		rel := "partial"
		u := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/post.yaml": hookDoc("post", "post-install", "post"),
			"templates/ok.yaml":   cmDoc("ok", "  k: v\n"+unlessEdited+"  gone: x\n{{- end }}\n"),
			"templates/fix.yaml":  cmDoc("fixed", "  k: {{ .Values.fixed | default \"v1\" | quote }}\n") + "immutable: true\n",
			"templates/new.yaml":  "{{- if .Values.edited }}\n" + cmDoc("new", "  k: v\n") + "{{- end }}\n",
			"templates/orph.yaml": unlessEdited + cmDoc("orphan", "  a: b\n") + "{{- end }}\n"})
		e.first(t, rel, u, bad)
		if _, err := e.retry(rel, u, map[string]any{"edited": true, "fixed": "v2"}); err == nil {
			t.Fatal("control: an immutable ConfigMap must refuse the change")
		}
		got := e.last(t, rel)
		if got.Status != helmconfig.StatusFailed || got.Revision != 2 {
			t.Fatalf("revision %d %s", got.Revision, got.Status)
		}
		for _, c := range []struct {
			text string
			want bool
		}{{rel + "-orphan", true}, {rel + "-new", true}, {"v1", true}, {"v2", false}, {"gone", false}} {
			if strings.Contains(strings.ReplaceAll(got.Manifest, "apiVersion: v1", ""), c.text) != c.want {
				t.Errorf("stored manifest contains %q = %v, want %v:\n%s", c.text, !c.want, c.want, got.Manifest)
			}
		}
		// The next render drops what only the partial attempt created, and what it could not delete.
		if _, err := e.retry(rel, u, map[string]any{"edited": true}); err != nil {
			t.Fatal(err)
		}
		if e.cm(t, rel+"-orphan") != nil || len(e.cm(t, rel+"-ok").Data) != 1 || e.cm(t, rel+"-new") == nil {
			t.Error("the retry after a partial apply did not converge")
		}
	})

	t.Run("a deletion the apiserver refuses stays in the record", func(t *testing.T) {
		rel := "nodel"
		e.first(t, rel, url, bad)
		rc := rest.CopyConfig(e.hc.restConfig)
		refused := 0
		rc.Wrap(func(next http.RoundTripper) http.RoundTripper {
			return roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, rel+"-orphan") {
					refused++
					return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}}, Request: r,
						Body: io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`))}, nil
				}
				return next.RoundTrip(r)
			})
		})
		op, err := e.retry(rel, url, edited, func(c *helmconfig.InstallConfig) { c.RestConfig = rc })
		if err != nil {
			t.Fatal(err)
		}
		if refused == 0 || e.cm(t, rel+"-orphan") == nil {
			t.Fatalf("control: the delete must be refused (refused=%d)", refused)
		}
		if !strings.Contains(op.Release.Manifest, rel+"-orphan") || e.last(t, rel).Manifest != op.Release.Manifest {
			t.Fatalf("the record lost the object that is still there:\n%s", op.Release.Manifest)
		}
		if _, err := e.hc.Reconcile(ctx, rel, url, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: edited}}); err != nil {
			t.Fatal(err)
		}
		if e.cm(t, rel+"-orphan") != nil {
			t.Error("the next reconcile must finish the deletion")
		}
	})

	t.Run("a live Job's changed template is refused before anything is written", func(t *testing.T) {
		rel := "job"
		u := serveFiles(t, map[string]string{"Chart.yaml": chartYAML,
			"templates/pre.yaml":  hookDoc("pre", "pre-install", "pre"),
			"templates/post.yaml": hookDoc("post", "post-install", "post"),
			"templates/ok.yaml":   cmDoc("ok", "  k: {{ .Values.k | default \"v1\" | quote }}\n"),
			"templates/j.yaml":    "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: {{ .Release.Name }}-j\n  namespace: default\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n      - name: c\n        image: {{ .Values.image | default \"a:1\" }}\n"})
		e.first(t, rel, u, bad)
		_, err := e.retry(rel, u, map[string]any{"k": "v2", "image": "a:2"})
		if !errors.Is(err, helmconfig.ErrJobTemplateChanged) {
			t.Fatalf("err = %v, want ErrJobTemplateChanged", err)
		}
		if got := e.last(t, rel); got.Revision != 1 || e.cm(t, rel+"-ok").Data["k"] != "v1" {
			t.Errorf("revision %d, ok=%v: something was written", got.Revision, e.cm(t, rel+"-ok").Data)
		}
		if _, err := e.retry(rel, u, map[string]any{"k": "v2"}); err != nil {
			t.Errorf("control: an unchanged Job must not be refused: %v", err)
		}
	})

	t.Run("a release that is not a failed first install", func(t *testing.T) {
		refuse := func(rel string) {
			t.Helper()
			before, _ := e.hc.History(ctx, rel, &helmconfig.HistoryConfig{})
			if _, err := e.retry(rel, url, edited); !errors.Is(err, helmconfig.ErrNotRetryableAsInstall) {
				t.Errorf("%s: err = %v, want ErrNotRetryableAsInstall", rel, err)
			}
			if after, _ := e.hc.History(ctx, rel, &helmconfig.HistoryConfig{}); len(after) != len(before) {
				t.Errorf("%s: a revision was written", rel)
			}
		}
		refuse("absent")
		if _, err := e.hc.Install(ctx, "dep", url, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
			t.Fatal(err)
		}
		refuse("dep")
		// A failed upgrade over a deployed revision: the latest is failed, but the release was live.
		u := serveFiles(t, map[string]string{"Chart.yaml": chartYAML, "templates/up.yaml": hookDoc("up", "pre-upgrade", "up")})
		if _, err := e.hc.Install(ctx, "depfail", u, &helmconfig.InstallConfig{Namespace: "default"}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.hc.Upgrade(ctx, "depfail", u, &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"up": "bad"}}}); err == nil {
			t.Fatal("control: the upgrade must fail")
		}
		if got := e.last(t, "depfail"); got.Status != helmconfig.StatusFailed {
			t.Fatalf("control: latest is %s", got.Status)
		}
		refuse("depfail")
		e.first(t, "pend", url, bad)
		ac, err := e.hc.newActionConfig(ctx, "default", e.hc.restConfig, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ac.Releases.Last("pend")
		if err != nil {
			t.Fatal(err)
		}
		r.Info.Status = release.StatusPendingInstall
		if err := ac.Releases.Update(r); err != nil {
			t.Fatal(err)
		}
		refuse("pend")
	})

	t.Run("another writer takes the next revision", func(t *testing.T) {
		rel := "race"
		e.first(t, rel, url, bad)
		ac, err := e.hc.newActionConfig(ctx, "default", e.hc.restConfig, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.retry(rel, url, edited, func(c *helmconfig.InstallConfig) {
			c.PostRenderer = beforeRender(func() {
				r, err := ac.Releases.Last(rel)
				if err != nil {
					t.Error(err)
					return
				}
				r.Version++
				if err := ac.Releases.Create(r); err != nil {
					t.Error(err)
				}
			})
		})
		if !errors.Is(err, helmconfig.ErrOperationInProgress) {
			t.Fatalf("err = %v, want ErrOperationInProgress", err)
		}
		if e.cm(t, rel+"-ok").Data["k"] != "v1" || e.cm(t, rel+"-orphan") == nil {
			t.Error("the refused attempt applied something")
		}
	})

	t.Run("the record is written before the post-install hooks", func(t *testing.T) {
		rel := "record"
		e.first(t, rel, url, bad)
		ac, err := e.hc.newActionConfig(ctx, "default", e.hc.restConfig, nil)
		if err != nil {
			t.Fatal(err)
		}
		rc := rest.CopyConfig(e.hc.restConfig)
		var atHook string
		rc.Wrap(func(next http.RoundTripper) http.RoundTripper {
			return roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/no-such-namespace/configmaps") {
					if l, err := ac.Releases.Last(rel); err == nil {
						atHook = l.Manifest
					}
				}
				return next.RoundTrip(r)
			})
		})
		if _, err := e.retry(rel, url, map[string]any{"edited": true, "post": "bad"}, func(c *helmconfig.InstallConfig) { c.RestConfig = rc }); err == nil {
			t.Fatal("control: the post-install hook must fail")
		}
		if atHook == "" || strings.Contains(atHook, rel+"-orphan") {
			t.Errorf("at the post-install hook the stored manifest is not what was applied:\n%s", atHook)
		}
	})

	t.Run("a dry run writes nothing", func(t *testing.T) {
		rel := "dry"
		e.first(t, rel, url, bad)
		op, err := e.retry(rel, url, edited, func(c *helmconfig.InstallConfig) { c.DryRun = helmconfig.DryRunServer })
		if err != nil || !strings.Contains(op.Release.Manifest, rel+"-installonly") {
			t.Fatalf("dry run: %v", err)
		}
		if e.last(t, rel).Revision != 1 || e.cm(t, rel+"-orphan") == nil || e.cm(t, rel+"-ok").Data["gone"] != "x" {
			t.Error("a dry run wrote something")
		}
	})

	t.Run("a cancelled attempt leaves a failed revision that can be retried", func(t *testing.T) {
		rel := "cancel"
		e.first(t, rel, url, bad)
		cctx, cancel := context.WithCancel(ctx)
		rc := rest.CopyConfig(e.hc.restConfig)
		done := make(chan struct{})
		rc.Wrap(func(next http.RoundTripper) http.RoundTripper {
			return roundTrip(func(r *http.Request) (*http.Response, error) {
				// The delete of the orphan is the last write of the objects step.
				if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, rel+"-orphan") {
					cancel()
					time.Sleep(time.Second) // let failRelease record first
					defer close(done)
				}
				return next.RoundTrip(r)
			})
		})
		_, err := e.hc.Install(cctx, rel, url, &helmconfig.InstallConfig{Namespace: "default", RetryFailed: true, RestConfig: rc,
			ActionConfig: &helmconfig.ActionConfig{Values: map[string]any{"edited": true, "post": "bad"}}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want canceled", err)
		}
		<-done
		time.Sleep(2 * time.Second)
		got := e.last(t, rel)
		if got.Status != helmconfig.StatusFailed || strings.Contains(got.Manifest, rel+"-orphan") {
			t.Fatalf("status %s, want failed with what was applied:\n%s", got.Status, got.Manifest)
		}
		if _, err := e.retry(rel, url, edited); err != nil {
			t.Fatal(err)
		}
	})
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// beforeRender is a post-renderer that runs f and changes nothing.
type beforeRender func()

func (f beforeRender) Run(b *bytes.Buffer) (*bytes.Buffer, error) { f(); return b, nil }
