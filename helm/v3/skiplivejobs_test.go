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

func jobInfo(name, ns string) *resource.Info {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
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

func TestSkipLiveJobs(t *testing.T) {
	t.Run("a Job present in BOTH live and target is skipped", func(t *testing.T) {
		current := kube.ResourceList{jobInfo("keystone-bootstrap", "openstack"), cmInfo("cfg", "openstack")}
		target := kube.ResourceList{jobInfo("keystone-bootstrap", "openstack"), cmInfo("cfg", "openstack")}

		skipped := skipLiveJobs(current, target)
		if len(skipped) != 1 {
			t.Fatalf("skipped = %v, want exactly the live Job. This is the case that wedged a0", skipped)
		}

		// Removed from BOTH sides, asserted through the PRODUCTION pairing. Calling withoutKeys
		// twice here is what let a mutant that filtered only target survive: the test was
		// verifying its own reconstruction instead of the caller's code.
		c2, t2, s2 := filterLiveJobs(current, target)
		if len(s2) != 1 {
			t.Fatalf("filterLiveJobs skipped %v, want one", s2)
		}
		if len(c2) != 1 || len(t2) != 1 {
			t.Errorf("after filtering: current=%d target=%d, want 1 and 1", len(c2), len(t2))
		}
		for _, l := range []kube.ResourceList{c2, t2} {
			for _, i := range l {
				if i.Object.GetObjectKind().GroupVersionKind().Kind == "Job" {
					t.Error("a Job survived the filter on one side; the merge will still patch it")
				}
			}
		}
	})

	// CONTROL 1. A Job that is rendered but NOT live must NOT be skipped — it has to be created,
	// and skipping it would silently stop the self-heal ever applying a new Job.
	t.Run("a Job in target but not live is NOT skipped", func(t *testing.T) {
		skipped := skipLiveJobs(kube.ResourceList{}, kube.ResourceList{jobInfo("fresh", "ns")})
		if len(skipped) != 0 {
			t.Errorf("skipped = %v; a Job that does not exist yet must still be created", skipped)
		}
	})

	// THE ARM MUTANT A DEMANDED. Matching on Kind alone — ignoring the group — survived every
	// other arm, because nothing here was a "Job" outside batch. A CRD may legitimately define a
	// kind named Job, and skipping it would silently stop the self-heal healing it.
	t.Run("a Job kind OUTSIDE the batch group is not skipped", func(t *testing.T) {
		foreign := func(name, ns string) *resource.Info {
			return &resource.Info{Name: name, Namespace: ns, Object: &unstructuredLike{
				gvk: schema.GroupVersionKind{Group: "pipelines.example.io", Version: "v1", Kind: "Job"},
			}}
		}
		current := kube.ResourceList{foreign("build", "ns")}
		target := kube.ResourceList{foreign("build", "ns")}
		if skipped := skipLiveJobs(current, target); len(skipped) != 0 {
			t.Errorf("skipped = %v; only batch/Job has the immutable pod template this guards "+
				"against, and a CRD kind named Job must keep healing", skipped)
		}
	})

	// CONTROL 2. Nothing else is skipped, or this fix quietly disables the self-heal for
	// everything — which would be a far worse defect than the one it fixes.
	t.Run("non-Job kinds are never skipped", func(t *testing.T) {
		current := kube.ResourceList{cmInfo("a", "ns"), cronInfo("sched", "ns")}
		target := kube.ResourceList{cmInfo("a", "ns"), cronInfo("sched", "ns")}
		if skipped := skipLiveJobs(current, target); len(skipped) != 0 {
			t.Errorf("skipped = %v; only batch/Job is immutable in the way this guards against. "+
				"A CronJob shares the batch group and is NOT run-once, so it must keep healing", skipped)
		}
	})

	// CONTROL 3. withoutKeys with nothing to drop must return the list untouched, or every
	// reconcile silently rebuilds its inputs.
	t.Run("withoutKeys is a no-op for an empty key set", func(t *testing.T) {
		in := kube.ResourceList{cmInfo("a", "ns"), jobInfo("b", "ns")}
		if out := withoutKeys(in, nil); len(out) != len(in) {
			t.Errorf("len = %d, want %d", len(out), len(in))
		}
	})

	t.Run("namespace is part of the identity", func(t *testing.T) {
		// Same name, different namespace: not the same object, must not be skipped.
		current := kube.ResourceList{jobInfo("bootstrap", "ns-a")}
		target := kube.ResourceList{jobInfo("bootstrap", "ns-b")}
		if skipped := skipLiveJobs(current, target); len(skipped) != 0 {
			t.Errorf("skipped = %v; a Job of the same name in another namespace is a different "+
				"object and its target must still be applied", skipped)
		}
	})
}
