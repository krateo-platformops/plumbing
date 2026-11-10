package helm

import (
	"testing"

	"helm.sh/helm/v3/pkg/kube"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

// #276. A Job's spec.template is immutable, so a three-way patch of a live Job whose template
// differs from the target's can never succeed — and because ONE error fails the whole self-heal
// apply, every other child of the composition stops converging too.
//
// Observed on the a0 cluster: six keystone Jobs left from release v0-3-16, re-created DURING the
// uninstall by a self-heal that had passed its deletion guard 252 lines earlier, wedged the
// v0-3-18 composition permanently at Synced=False/ReconcileError. Their creating field manager was
// "controller", not "helm", which is what attributed them to the self-heal rather than to a helm
// install.

func jobInfo(name, ns string) *resource.Info { return jobInfoCmd(name, ns, "echo v1") }

// jobInfoCmd is a Job whose pod template runs cmd, so two renders can differ in the template alone.
func jobInfoCmd(name, ns, cmd string) *resource.Info {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	j.Spec.Template.Spec.Containers = []corev1.Container{{Name: "c", Image: "busybox:1.36", Command: []string{"sh", "-c", cmd}}}
	j.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"})
	return &resource.Info{Name: name, Namespace: ns, Object: j}
}

func cmInfo(name, ns string) *resource.Info {
	c := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	c.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	return &resource.Info{Name: name, Namespace: ns, Object: c}
}

func cronInfo(name, ns string) *resource.Info {
	o := &unstructuredLike{gvk: schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"}}
	return &resource.Info{Name: name, Namespace: ns, Object: o}
}

// unstructuredLike is the smallest runtime.Object that reports a GVK, so the group/kind match can
// be tested without depending on a typed CronJob being registered.
type unstructuredLike struct {
	gvk schema.GroupVersionKind
}

func (u *unstructuredLike) GetObjectKind() schema.ObjectKind                { return u }
func (u *unstructuredLike) DeepCopyObject() runtime.Object                  { c := *u; return &c }
func (u *unstructuredLike) SetGroupVersionKind(gvk schema.GroupVersionKind) { u.gvk = gvk }
func (u *unstructuredLike) GroupVersionKind() schema.GroupVersionKind       { return u.gvk }

// liveOf marks every entry of list as present on the cluster, keyed as Reconcile keys liveObj.
func liveOf(list ...*resource.Info) map[string]runtime.Object {
	m := map[string]runtime.Object{}
	for _, i := range list {
		m[infoKey(i)] = i.Object
	}
	return m
}

func TestClassifyStoredJobs(t *testing.T) {
	t.Run("a Job in BOTH stored and target with the same template is skipped", func(t *testing.T) {
		current := kube.ResourceList{jobInfo("keystone-bootstrap", "openstack"), cmInfo("cfg", "openstack")}
		target := kube.ResourceList{jobInfo("keystone-bootstrap", "openstack"), cmInfo("cfg", "openstack")}

		got := classifyStoredJobs(current, target, liveOf(target...))
		if len(got.skip) != 1 || len(got.templateChanged) != 0 {
			t.Fatalf("skip=%v templateChanged=%v, want the Job skipped and nothing reported", got.skip, got.templateChanged)
		}
		// Removed from BOTH sides through the same withoutKeys call Reconcile makes with got.skip.
		c2, t2 := withoutKeys(current, got.skip), withoutKeys(target, got.skip)
		if len(c2) != 1 || len(t2) != 1 {
			t.Errorf("after filtering: current=%d target=%d, want 1 and 1", len(c2), len(t2))
		}
	})

	// core-provider#403, arm A: the CHART changed a live Job's template. Skipping it silently made
	// Reconcile report convergence on a release that was never applied. It is still kept out of the
	// merge (the patch cannot succeed) AND reported.
	t.Run("a live Job whose template the chart changed is skipped AND reported", func(t *testing.T) {
		current := kube.ResourceList{jobInfoCmd("probe-plain-job", "ns", "echo v1")}
		target := kube.ResourceList{jobInfoCmd("probe-plain-job", "ns", "echo v2")}
		got := classifyStoredJobs(current, target, liveOf(target...))
		if len(got.skip) != 1 {
			t.Errorf("skip=%v, want the Job kept out of the merge: patching it can only fail", got.skip)
		}
		if len(got.templateChanged) != 1 || got.templateChanged[0] != "ns/probe-plain-job" {
			t.Errorf("templateChanged=%v, want [ns/probe-plain-job]", got.templateChanged)
		}
	})

	// The inverse: a changed Job that is NOT live (deleted by ttlSecondsAfterFinished, or by hand)
	// stays in the merge, which creates it with the new template. Reporting it would be a false
	// Synced=False for a release that can be applied.
	t.Run("a changed Job that is not live stays in the merge", func(t *testing.T) {
		current := kube.ResourceList{jobInfoCmd("j", "ns", "echo v1")}
		target := kube.ResourceList{jobInfoCmd("j", "ns", "echo v2")}
		got := classifyStoredJobs(current, target, liveOf())
		if len(got.skip) != 0 || len(got.templateChanged) != 0 {
			t.Errorf("skip=%v templateChanged=%v, want neither: it can simply be created", got.skip, got.templateChanged)
		}
	})

	// CONTROL 1. A Job rendered but absent from the stored release is never skipped: it has to be
	// created, and skipping it would silently stop the self-heal ever applying a new Job.
	t.Run("a Job in target but not in the stored release is NOT skipped", func(t *testing.T) {
		target := kube.ResourceList{jobInfo("fresh", "ns")}
		got := classifyStoredJobs(kube.ResourceList{}, target, liveOf(target...))
		if len(got.skip) != 0 || len(got.templateChanged) != 0 {
			t.Errorf("skip=%v templateChanged=%v; a Job new to this release must still be applied", got.skip, got.templateChanged)
		}
	})

	// Matching on Kind alone, ignoring the group, survived every other arm once. A CRD may
	// legitimately define a kind named Job, and skipping it would stop the self-heal healing it.
	t.Run("a Job kind OUTSIDE the batch group is not skipped", func(t *testing.T) {
		foreign := func(name, ns string) *resource.Info {
			return &resource.Info{Name: name, Namespace: ns, Object: &unstructuredLike{
				gvk: schema.GroupVersionKind{Group: "pipelines.example.io", Version: "v1", Kind: "Job"},
			}}
		}
		current := kube.ResourceList{foreign("build", "ns")}
		target := kube.ResourceList{foreign("build", "ns")}
		if got := classifyStoredJobs(current, target, liveOf(target...)); len(got.skip) != 0 || len(got.templateChanged) != 0 {
			t.Errorf("skip=%v templateChanged=%v; only batch/Job has the immutable pod template", got.skip, got.templateChanged)
		}
	})

	// CONTROL 2. Nothing else is skipped, or this quietly disables the self-heal for everything.
	t.Run("non-Job kinds are never skipped", func(t *testing.T) {
		current := kube.ResourceList{cmInfo("a", "ns"), cronInfo("sched", "ns")}
		target := kube.ResourceList{cmInfo("a", "ns"), cronInfo("sched", "ns")}
		if got := classifyStoredJobs(current, target, liveOf(target...)); len(got.skip) != 0 {
			t.Errorf("skip=%v; a CronJob shares the batch group and is NOT run-once, so it must keep healing", got.skip)
		}
	})

	// CONTROL 3. withoutKeys with nothing to drop returns the list untouched.
	t.Run("withoutKeys is a no-op for an empty key set", func(t *testing.T) {
		in := kube.ResourceList{cmInfo("a", "ns"), jobInfo("b", "ns")}
		if out := withoutKeys(in, nil); len(out) != len(in) {
			t.Errorf("len = %d, want %d", len(out), len(in))
		}
	})

	t.Run("namespace is part of the identity", func(t *testing.T) {
		current := kube.ResourceList{jobInfo("bootstrap", "ns-a")}
		target := kube.ResourceList{jobInfoCmd("bootstrap", "ns-b", "echo v2")}
		if got := classifyStoredJobs(current, target, liveOf(target...)); len(got.skip) != 0 || len(got.templateChanged) != 0 {
			t.Errorf("skip=%v templateChanged=%v; a Job of the same name in another namespace is a different object", got.skip, got.templateChanged)
		}
	})
}
