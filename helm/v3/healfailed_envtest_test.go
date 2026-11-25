//go:build envtest

package helm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// healChart: ConfigMaps a (k from values) and b; "new" only when k is v2; immutable "imm" (k from
// values); a pre-upgrade hook that always succeeds and is re-created each time hooks run ("ran"), and
// pre- and post-upgrade hooks that fail while .Values.pre / .Values.post is "bad".
func healChart() map[string]string {
	return map[string]string{
		"Chart.yaml":         chartYAML,
		"templates/a.yaml":   cmDoc("a", "  k: {{ .Values.k | default \"v1\" | quote }}\n"),
		"templates/b.yaml":   cmDoc("b", "  k: b\n"),
		"templates/new.yaml": "{{- if eq (.Values.k | default \"v1\") \"v2\" }}\n" + cmDoc("new", "  k: new\n") + "{{- end }}\n",
		"templates/imm.yaml": "{{- if .Values.imm }}\n" + cmDoc("imm", "  k: {{ .Values.k | default \"v1\" | quote }}\n") + "immutable: true\n{{- end }}\n",
		"templates/ran.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-ran\n  namespace: default\n" +
			"  annotations:\n    helm.sh/hook: pre-upgrade\n    helm.sh/hook-weight: \"-5\"\n    helm.sh/hook-delete-policy: before-hook-creation\ndata:\n  k: v\n",
		"templates/pre.yaml":  hookDoc("pre", "pre-upgrade", "pre"),
		"templates/post.yaml": hookDoc("post", "post-upgrade", "post"),
		"templates/tr.yaml": "{{- if .Values.trace }}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-tr\n  namespace: default\n" +
			"  annotations:\n    krateo.io/traceparent: 00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-aaaaaaaaaaaaaaaa-01\ndata:\n  k: tr\n{{- end }}\n",
		"templates/job.yaml": "{{- if .Values.job }}\napiVersion: batch/v1\nkind: Job\nmetadata:\n  name: {{ .Release.Name }}-job\n  namespace: default\n" +
			"spec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n        - name: c\n          image: busybox:1.36\n          command: [\"sh\", \"-c\", \"echo a\"]\n{{- end }}\n",
		"templates/preinst.yaml":  hookDoc("preinst", "pre-install", "preinst"),
		"templates/postinst.yaml": hookDoc("postinst", "post-install", "postinst"),
	}
}

type healEnv struct {
	*retryEnv
	url string
}

func (e *healEnv) install(t *testing.T, rel string, vals map[string]any) {
	t.Helper()
	if _, err := e.hc.Install(context.Background(), rel, e.url, &helmconfig.InstallConfig{Namespace: "default",
		ActionConfig: &helmconfig.ActionConfig{Values: vals}}); err != nil {
		t.Fatalf("install: %v", err)
	}
}

func (e *healEnv) upgrade(rel string, vals map[string]any) error {
	_, err := e.hc.Upgrade(context.Background(), rel, e.url, &helmconfig.UpgradeConfig{MaxHistory: 3,
		ActionConfig: &helmconfig.ActionConfig{Values: vals}})
	return err
}

var errNotRecorded = errors.New("not recorded")

// heal runs Reconcile with the request vals, stopping before the retry (BeforeRevision fails), so what
// changed is the heal's alone.
func (e *healEnv) heal(t *testing.T, rel string, vals map[string]any) *helmconfig.ReconcileResult {
	t.Helper()
	res, err := e.hc.Reconcile(context.Background(), rel, e.url, &helmconfig.UpgradeConfig{MaxHistory: 3,
		ActionConfig:   &helmconfig.ActionConfig{Values: vals},
		BeforeRevision: func(context.Context) error { return errNotRecorded }})
	if !errors.Is(err, errNotRecorded) || res == nil || !res.RetriedFailedRelease || res.FailedReleaseHeal == nil {
		t.Fatalf("heal: err=%v res=%+v", err, res)
	}
	return res
}

// raw is the latest revision as stored, labels included.
func (e *healEnv) raw(t *testing.T, rel string) *release.Release {
	t.Helper()
	ac, err := e.hc.newActionConfig(context.Background(), "default", e.hc.restConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	last, err := ac.Releases.Last(rel)
	if err != nil {
		t.Fatal(err)
	}
	return last
}

func (e *healEnv) data(t *testing.T, name string) string {
	t.Helper()
	cm := e.cm(t, name)
	if cm == nil {
		return "<absent>"
	}
	return cm.Data["k"]
}

func (e *healEnv) uid(t *testing.T, name string) types.UID {
	t.Helper()
	cm := e.cm(t, name)
	if cm == nil {
		return ""
	}
	return cm.UID
}

func (e *healEnv) edit(t *testing.T, name, k string) {
	t.Helper()
	cm := e.cm(t, name)
	cm.Data["k"] = k
	if _, err := e.kube.CoreV1().ConfigMaps("default").Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (e *healEnv) remove(t *testing.T, name string) {
	t.Helper()
	if err := e.kube.CoreV1().ConfigMaps("default").Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestEnvtest_HealFailedRelease(t *testing.T) {
	e := &healEnv{retryEnv: newRetryEnv(t)}
	e.url = serveFiles(t, healChart())
	v1 := map[string]any{}
	preBad := map[string]any{"k": "v2", "pre": "bad"}
	postBad := map[string]any{"k": "v2", "post": "bad"}

	t.Run("pre-upgrade hook failed: healed to the last deployed objects, no hook, nothing of the render", func(t *testing.T) {
		rel := "hpre"
		e.install(t, rel, v1)
		if err := e.upgrade(rel, preBad); err == nil {
			t.Fatal("control: the pre-upgrade hook must fail")
		}
		last := e.raw(t, rel)
		if last.Info.Status != release.StatusFailed || !hasAppliedRecord(last) {
			t.Fatalf("revision %d: status %s, recorded %v", last.Version, last.Info.Status, hasAppliedRecord(last))
		}
		if e.data(t, rel+"-a") != "v1" || e.data(t, rel+"-new") != "<absent>" {
			t.Fatalf("control: the failed attempt applied something: a=%s new=%s", e.data(t, rel+"-a"), e.data(t, rel+"-new"))
		}
		ran := e.uid(t, rel+"-ran")
		if ran == "" {
			t.Fatal("control: the failed attempt's first hook must have run")
		}

		e.edit(t, rel+"-a", "drift")
		e.remove(t, rel+"-b")
		res := e.heal(t, rel, preBad)
		h := res.FailedReleaseHeal
		if !h.Recorded || h.Revision != last.Version || h.Created != 1 || h.PatchedUpdated != 1 || h.Err != nil {
			t.Errorf("heal = %+v", h)
		}
		if got := e.data(t, rel+"-a"); got != "v1" {
			t.Errorf("a = %q, want v1 (the last applied document)", got)
		}
		if got := e.data(t, rel+"-b"); got != "b" {
			t.Errorf("b = %q, want recreated", got)
		}
		if got := e.data(t, rel+"-new"); got != "<absent>" {
			t.Errorf("new = %q: the heal applied the failed render", got)
		}
		if e.uid(t, rel+"-ran") != ran {
			t.Error("a hook ran during the heal")
		}
		if !res.Changed || res.Created != 1 || res.PatchedUpdated != 1 || len(res.Objects) != 2 {
			t.Errorf("result: changed=%v created=%d patched=%d objects=%d", res.Changed, res.Created, res.PatchedUpdated, len(res.Objects))
		}

		// Steady state: nothing more to heal, and the retry fails in its hook again without applying.
		e.edit(t, rel+"-a", "drift2")
		res, err := e.hc.Reconcile(context.Background(), rel, e.url, &helmconfig.UpgradeConfig{MaxHistory: 3,
			ActionConfig: &helmconfig.ActionConfig{Values: preBad}})
		if err == nil {
			t.Fatal("control: the retry's pre-upgrade hook must fail")
		}
		if e.data(t, rel+"-a") != "v1" || e.data(t, rel+"-new") != "<absent>" {
			t.Errorf("after the retry: a=%s new=%s", e.data(t, rel+"-a"), e.data(t, rel+"-new"))
		}
		// A heal that changed something writes a revision: the retry's.
		if !res.Changed || res.AppliedPartially || e.raw(t, rel).Version != last.Version+1 {
			t.Errorf("retry: changed=%v partial=%v revision=%d", res.Changed, res.AppliedPartially, e.raw(t, rel).Version)
		}
		if again := e.heal(t, rel, preBad); again.FailedReleaseHeal.Created+again.FailedReleaseHeal.PatchedUpdated != 0 || again.Changed {
			t.Errorf("steady state healed again: %+v", again.FailedReleaseHeal)
		}
	})

	t.Run("post-upgrade hook failed: healed to the new objects", func(t *testing.T) {
		rel := "hpost"
		e.install(t, rel, v1)
		if err := e.upgrade(rel, postBad); err == nil {
			t.Fatal("control: the post-upgrade hook must fail")
		}
		if !hasAppliedRecord(e.raw(t, rel)) || e.data(t, rel+"-a") != "v2" || e.data(t, rel+"-new") != "new" {
			t.Fatalf("control: a=%s new=%s", e.data(t, rel+"-a"), e.data(t, rel+"-new"))
		}
		e.edit(t, rel+"-a", "drift")
		e.remove(t, rel+"-new")
		h := e.heal(t, rel, postBad).FailedReleaseHeal
		if !h.Recorded || h.Created != 1 || h.PatchedUpdated != 1 {
			t.Errorf("heal = %+v", h)
		}
		if e.data(t, rel+"-a") != "v2" || e.data(t, rel+"-new") != "new" {
			t.Errorf("a=%s new=%s, want v2 and new", e.data(t, rel+"-a"), e.data(t, rel+"-new"))
		}
	})

	t.Run("partial apply: each object healed to its last confirmed document", func(t *testing.T) {
		rel := "hpart"
		e.install(t, rel, map[string]any{"imm": true})
		// imm is immutable, so its write fails and a's succeeds.
		if err := e.upgrade(rel, map[string]any{"imm": true, "k": "v3"}); err == nil {
			t.Fatal("control: the immutable ConfigMap's write must fail")
		}
		if !hasAppliedRecord(e.raw(t, rel)) || e.data(t, rel+"-a") != "v3" || e.data(t, rel+"-imm") != "v1" {
			t.Fatalf("control: recorded=%v a=%s imm=%s", hasAppliedRecord(e.raw(t, rel)), e.data(t, rel+"-a"), e.data(t, rel+"-imm"))
		}
		e.edit(t, rel+"-a", "drift")
		e.remove(t, rel+"-imm")
		h := e.heal(t, rel, map[string]any{"imm": true, "k": "v3"}).FailedReleaseHeal
		if !h.Recorded || h.Created != 1 || h.PatchedUpdated != 1 {
			t.Errorf("heal = %+v", h)
		}
		if e.data(t, rel+"-a") != "v3" || e.data(t, rel+"-imm") != "v1" {
			t.Errorf("a=%s imm=%s, want v3 (written) and v1 (its write failed)", e.data(t, rel+"-a"), e.data(t, rel+"-imm"))
		}
	})

	t.Run("four failed retries in a row, history 3: the record outlives the revision that made it", func(t *testing.T) {
		rel := "hhist"
		e.install(t, rel, v1)
		// The record is made here: the objects step ran, so it is the render (a=v2 and new).
		if err := e.upgrade(rel, postBad); err == nil {
			t.Fatal("control: the post-upgrade hook must fail")
		}
		made := e.raw(t, rel).Version
		// Then a pre-upgrade hook fails five times; each revision carries the record forward.
		if err := e.upgrade(rel, preBad); err == nil {
			t.Fatal("control: the pre-upgrade hook must fail")
		}
		for i := 0; i < 4; i++ {
			if _, err := e.hc.Reconcile(context.Background(), rel, e.url, &helmconfig.UpgradeConfig{MaxHistory: 3,
				ActionConfig: &helmconfig.ActionConfig{Values: preBad}}); err == nil {
				t.Fatalf("retry %d: the pre-upgrade hook must fail", i)
			}
		}
		history, err := e.hc.History(context.Background(), rel, &helmconfig.HistoryConfig{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range history {
			if r.Revision == made {
				t.Fatalf("control: revision %d, which made the record, must have aged out (%d revisions)", made, len(history))
			}
		}
		e.edit(t, rel+"-a", "drift")
		e.remove(t, rel+"-new")
		h := e.heal(t, rel, preBad).FailedReleaseHeal
		if !h.Recorded || h.Revision != e.raw(t, rel).Version || h.Created != 1 || h.PatchedUpdated != 1 {
			t.Errorf("heal = %+v", h)
		}
		if e.data(t, rel+"-a") != "v2" || e.data(t, rel+"-new") != "new" {
			t.Errorf("a=%s new=%s, want v2 and new (the record of revision %d)", e.data(t, rel+"-a"), e.data(t, rel+"-new"), made)
		}
	})

	t.Run("a failed revision helm alone wrote is not healed", func(t *testing.T) {
		rel := "hhelm"
		e.install(t, rel, v1)
		helmAloneUpgrade(t, e, rel, preBad)
		if hasAppliedRecord(e.raw(t, rel)) {
			t.Fatal("control: helm alone must not mark the revision")
		}
		e.remove(t, rel+"-b")
		res := e.heal(t, rel, preBad)
		if h := res.FailedReleaseHeal; h.Recorded || h.Created != 0 || res.Changed {
			t.Errorf("heal = %+v changed=%v", h, res.Changed)
		}
		if e.data(t, rel+"-b") != "<absent>" {
			t.Error("healed against a revision with no record")
		}
		// The next attempt through plumbing fails in its hook too: its baseline is unknown, so it is not
		// marked either.
		if err := e.upgrade(rel, preBad); err == nil {
			t.Fatal("control: the pre-upgrade hook must fail")
		}
		if hasAppliedRecord(e.raw(t, rel)) {
			t.Error("a guess was marked as a record")
		}
		// One that gets past its objects step knows what it applied.
		if err := e.upgrade(rel, postBad); err == nil {
			t.Fatal("control: the post-upgrade hook must fail")
		}
		if !hasAppliedRecord(e.raw(t, rel)) {
			t.Error("an attempt whose objects step ran must be recorded")
		}
	})

	t.Run("objects only a failed attempt applied are deleted when the render drops them (plumbing#232)", func(t *testing.T) {
		rel := "hdrop"
		e.install(t, rel, v1)
		if err := e.upgrade(rel, postBad); err == nil {
			t.Fatal("control: the post-upgrade hook must fail")
		}
		if e.data(t, rel+"-new") != "new" {
			t.Fatal("control: the failed attempt must have applied new")
		}
		if err := e.upgrade(rel, map[string]any{"k": "v3"}); err != nil {
			t.Fatal(err)
		}
		if got := e.data(t, rel+"-new"); got != "<absent>" {
			t.Errorf("new = %q: left behind by the failed attempt", got)
		}
		if last := e.raw(t, rel); last.Info.Status != release.StatusDeployed || !hasAppliedRecord(last) {
			t.Errorf("revision %d: %s, recorded %v", last.Version, last.Info.Status, hasAppliedRecord(last))
		}
	})

	t.Run("a dropped object that is gone, or not this release's, is left alone", func(t *testing.T) {
		rel := "hgone"
		e.install(t, rel, v1)
		if err := e.upgrade(rel, map[string]any{"k": "v2", "post": "bad", "imm": true}); err == nil {
			t.Fatal("control: the post-upgrade hook must fail")
		}
		// new is gone; imm now belongs to somebody else.
		e.remove(t, rel+"-new")
		imm := e.cm(t, rel+"-imm")
		delete(imm.Annotations, helmReleaseNameAnnotation)
		if _, err := e.kube.CoreV1().ConfigMaps("default").Update(context.Background(), imm, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := e.upgrade(rel, map[string]any{"k": "v3"}); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		if e.data(t, rel+"-imm") != "v2" {
			t.Errorf("imm = %s: deleted though it is not this release's", e.data(t, rel+"-imm"))
		}
	})

	t.Run("a live object of this release that the record does not name is adopted", func(t *testing.T) {
		rel := "hadopt"
		e.install(t, rel, v1)
		if err := e.upgrade(rel, preBad); err == nil {
			t.Fatal("control: the pre-upgrade hook must fail")
		}
		// new is live and this release's, but no applied record names it.
		if _, err := e.kube.CoreV1().ConfigMaps("default").Create(context.Background(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: rel + "-new", Namespace: "default",
				Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm"},
				Annotations: map[string]string{helmReleaseNameAnnotation: rel, helmReleaseNamespaceAnnotation: "default"}},
			Data: map[string]string{"k": "other"},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := e.upgrade(rel, map[string]any{"k": "v2"}); err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		if e.data(t, rel+"-new") != "new" {
			t.Errorf("new = %s, want the render", e.data(t, rel+"-new"))
		}
	})

	t.Run("a partial apply over an unknown baseline is not marked", func(t *testing.T) {
		rel := "hunk"
		e.install(t, rel, map[string]any{"imm": true})
		helmAloneUpgrade(t, e, rel, map[string]any{"imm": true, "k": "v2", "pre": "bad"})
		if err := e.upgrade(rel, map[string]any{"imm": true, "k": "v3"}); err == nil {
			t.Fatal("control: the immutable ConfigMap's write must fail")
		}
		if e.data(t, rel+"-a") != "v3" {
			t.Fatal("control: a's write must have succeeded")
		}
		if hasAppliedRecord(e.raw(t, rel)) {
			t.Error("a partial apply over an unknown baseline was marked: the failed writes' documents are unknown")
		}
	})

	t.Run("trace-context stamps a child carries are not healed", func(t *testing.T) {
		rel := "htrace"
		vals := map[string]any{"trace": true}
		e.install(t, rel, vals)
		if err := e.upgrade(rel, map[string]any{"trace": true, "k": "v2", "pre": "bad"}); err == nil {
			t.Fatal("control: the pre-upgrade hook must fail")
		}
		cm := e.cm(t, rel+"-tr")
		cm.Annotations[traceparentAnnotation] = "00-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bbbbbbbbbbbbbbbb-01"
		if _, err := e.kube.CoreV1().ConfigMaps("default").Update(context.Background(), cm, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		h := e.heal(t, rel, map[string]any{"trace": true, "k": "v2", "pre": "bad"}).FailedReleaseHeal
		if h.Created+h.PatchedUpdated != 0 || e.cm(t, rel+"-tr").Annotations[traceparentAnnotation] != cm.Annotations[traceparentAnnotation] {
			t.Errorf("heal = %+v: patched the trace stamp, so every pass of a failing release would", h)
		}
	})

	t.Run("a live Job is left alone", func(t *testing.T) {
		rel := "hjob"
		vals := map[string]any{"job": true}
		e.install(t, rel, vals)
		if err := e.upgrade(rel, map[string]any{"job": true, "k": "v2", "pre": "bad"}); err == nil {
			t.Fatal("control: the pre-upgrade hook must fail")
		}
		// Somebody re-created the Job with another pod template, which a patch can never change.
		ctx := context.Background()
		job, err := e.kube.BatchV1().Jobs("default").Get(ctx, rel+"-job", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		bg := metav1.DeletePropagationBackground
		if err := e.kube.BatchV1().Jobs("default").Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			if _, err := e.kube.BatchV1().Jobs("default").Get(ctx, job.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		again := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: "default", Labels: job.Labels, Annotations: job.Annotations},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{Name: "c", Image: "busybox:1.36", Command: []string{"sh", "-c", "echo b"}}}}}}}
		if _, err := e.kube.BatchV1().Jobs("default").Create(ctx, again, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		h := e.heal(t, rel, map[string]any{"job": true, "k": "v2", "pre": "bad"}).FailedReleaseHeal
		if h.Err != nil || h.PatchedUpdated != 0 {
			t.Errorf("heal = %+v: a live Job must be left out of the merge", h)
		}
	})

	t.Run("a retried first install is marked only when it knows what it applied", func(t *testing.T) {
		rel := "hinst"
		e.first(t, rel, e.url, map[string]any{"preinst": "bad"})
		if _, err := e.retry(rel, e.url, map[string]any{"preinst": "bad"}); err == nil {
			t.Fatal("control: the pre-install hook must fail")
		}
		if hasAppliedRecord(e.raw(t, rel)) {
			t.Error("a retry over a plain install's render, failed in its pre-install hook, was marked")
		}
		if _, err := e.retry(rel, e.url, map[string]any{"postinst": "bad"}); err == nil {
			t.Fatal("control: the post-install hook must fail")
		}
		if !hasAppliedRecord(e.raw(t, rel)) {
			t.Error("a retry whose objects step ran must be marked")
		}
	})
}

// A field the apiserver pruned while the CRD did not declare it, declared while the release is failed,
// is healed to the last applied document.
func TestEnvtest_HealFailedReleasePrunedField(t *testing.T) {
	e := &healEnv{retryEnv: newRetryEnv(t)}
	ctx := context.Background()
	ext, err := apiextclient.NewForConfig(e.hc.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	gvr := schema.GroupVersionResource{Group: "heal.test", Version: "v1", Resource: "widgets"}
	props := map[string]apiextensionsv1.JSONSchemaProps{"size": {Type: "string"}}
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.heal.test"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "heal.test", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{"spec": {Type: "object", Properties: props}}}}}},
		},
	}
	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEstablished(t, ext, crd.Name)

	files := healChart()
	files["templates/widget.yaml"] = "apiVersion: heal.test/v1\nkind: Widget\nmetadata:\n  name: {{ .Release.Name }}-w\n  namespace: default\nspec:\n  size: s\n  extra: x\n"
	e.url = serveFiles(t, files)
	rel := "hcrd"
	e.install(t, rel, map[string]any{})
	extra := func() (string, bool) {
		w, err := e.dyn.Resource(gvr).Namespace("default").Get(ctx, rel+"-w", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		v, ok, _ := unstructured.NestedString(w.Object, "spec", "extra")
		return v, ok
	}
	if _, ok := extra(); ok {
		t.Fatal("control: spec.extra must be pruned while the CRD does not declare it")
	}
	if err := e.upgrade(rel, map[string]any{"k": "v2", "pre": "bad"}); err == nil {
		t.Fatal("control: the pre-upgrade hook must fail")
	}

	cur, err := ext.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["extra"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The apiserver serves the new schema asynchronously; retry the heal until it takes.
	for i := 0; i < 50; i++ {
		e.heal(t, rel, map[string]any{"k": "v2", "pre": "bad"})
		if v, ok := extra(); ok {
			if v != "x" {
				t.Errorf("spec.extra = %q, want x", v)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Error("spec.extra was never healed to the last applied document")
}

// helmAloneUpgrade is an upgrade by helm without plumbing (as an earlier plumbing release, or the
// helm CLI, makes one); it must fail.
func helmAloneUpgrade(t *testing.T, e *healEnv, rel string, vals map[string]any) {
	t.Helper()
	ac, err := e.hc.newActionConfig(context.Background(), "default", e.hc.restConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := loader.LoadFiles(chartFiles(healChart()))
	if err != nil {
		t.Fatal(err)
	}
	up := action.NewUpgrade(ac)
	up.Namespace = "default"
	if _, err := up.Run(rel, ch, vals); err == nil {
		t.Fatal("control: helm's own upgrade must fail")
	}
}

// chartFiles is files as helm's loader takes them, for an upgrade run with helm alone.
func chartFiles(files map[string]string) []*loader.BufferedFile {
	var out []*loader.BufferedFile
	for name, body := range files {
		out = append(out, &loader.BufferedFile{Name: name, Data: []byte(body)})
	}
	return out
}

// A record stays one when Reconcile repairs it (step 1.5): a never-deployed release whose recorded
// failed revision names a kind the cluster stopped serving.
func TestEnvtest_HealFailedReleaseRepairedRecord(t *testing.T) {
	e := &healEnv{retryEnv: newRetryEnv(t)}
	ctx := context.Background()
	ext, err := apiextclient.NewForConfig(e.hc.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "gizmos.heal.test"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "heal.test", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "gizmos", Singular: "gizmo", Kind: "Gizmo", ListKind: "GizmoList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object",
					XPreserveUnknownFields: ptrTo(true)}}}},
		},
	}
	if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEstablished(t, ext, crd.Name)

	files := healChart()
	// Rendered while the kind is served, so the request's values stay the same once it is not.
	files["templates/gizmo.yaml"] = "{{- if .Capabilities.APIVersions.Has \"heal.test/v1/Gizmo\" }}\napiVersion: heal.test/v1\nkind: Gizmo\nmetadata:\n  name: {{ .Release.Name }}-g\n  namespace: default\n{{- end }}\n"
	e.url = serveFiles(t, files)
	rel := "hrep"
	e.first(t, rel, e.url, map[string]any{"postinst": "bad"})
	if _, err := e.retry(rel, e.url, map[string]any{"postinst": "bad"}); err == nil {
		t.Fatal("control: the post-install hook must fail")
	}
	if !hasAppliedRecord(e.raw(t, rel)) || !strings.Contains(e.raw(t, rel).Manifest, "kind: Gizmo") {
		t.Fatal("control: the retry ran its objects step, so it is recorded, Gizmo included")
	}

	if err := ext.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, crd.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := ext.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.remove(t, rel+"-b")
	// The kind is gone from discovery a little after the CRD; heal until the repair has run.
	for i := 0; i < 50; i++ {
		res, err := e.hc.Reconcile(ctx, rel, e.url, &helmconfig.UpgradeConfig{MaxHistory: 3,
			ActionConfig:   &helmconfig.ActionConfig{Values: map[string]any{"postinst": "bad"}},
			BeforeRevision: func(context.Context) error { return errNotRecorded }})
		if errors.Is(err, errNotRecorded) && res != nil && res.FailedReleaseHeal != nil {
			if !res.FailedReleaseHeal.Recorded {
				t.Fatalf("the repaired record lost its mark: %+v", res.FailedReleaseHeal)
			}
			if e.data(t, rel+"-b") != "b" {
				t.Errorf("b = %s, want healed", e.data(t, rel+"-b"))
			}
			if strings.Contains(e.raw(t, rel).Manifest, "kind: Gizmo") {
				t.Error("control: the repair must have dropped the unserved kind")
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("Reconcile never reached the heal once the kind was unserved")
}

func ptrTo[T any](v T) *T { return &v }
