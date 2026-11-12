package helm

import (
	"errors"
	"github.com/krateo-platformops/plumbing/kubeutil/content"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

// plumbing#135. Install, Upgrade and Uninstall report one verdict per child, built from the writes
// helm SENT through the KubeClient, with Reconcile's classification. These pin each write kind's
// verdicts, the hook exclusion, the kept children of an uninstall, and that the recorder is a
// faithful KubeClient (helm's type assertions still succeed through it).

func cmI(name, color string) *resource.Info { return vinfo(cm(name, color), "configmaps") }

func storedInfo(name, color string) *resource.Info {
	return vinfo(stored(cm(name, color)), "configmaps")
}

func hookInfo(name string) *resource.Info {
	return vinfo(withAnnotations(vobj("batch/v1", "Job", "ns", name, nil), map[string]string{"helm.sh/hook": "post-install"}), "jobs")
}

// changes lists each verdict as "name:change:verb", in the (sorted) order returned.
func changes(vs []helmconfig.ObjectVerdict) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name+":"+string(v.Change)+":"+v.Verb)
	}
	return out
}

func notFoundErr(name string) error {
	return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
}

func TestOperationVerdictsPerWrite(t *testing.T) {
	noGet := func(*resource.Info) (runtime.Object, error) { return nil, errors.New("not expected") }
	for _, tc := range []struct {
		name       string
		writes     []kubeWrite
		get        func(*resource.Info) (runtime.Object, error)
		want       []string
		wantHooks  int
		wantNotAtt int
	}{
		{
			// helm's perform attempts every object; a write the server confirmed is refreshed (it
			// carries a resourceVersion), so an unrefreshed one failed.
			name:   "a create: refreshed objects are created, the rest createFailed",
			writes: []kubeWrite{{op: opCreate, resources: kube.ResourceList{storedInfo("a", "red"), cmI("b", "red")}}},
			get:    noGet,
			want:   []string{"a:created:create", "b:createFailed:create"},
		},
		{
			name: "an update that changes content is specChanged, sent as patch",
			writes: []kubeWrite{{op: opUpdate,
				original: kube.ResourceList{cmI("c", "red")}, target: kube.ResourceList{storedInfo("c", "blue")},
				live:   map[string]runtime.Object{infoKey(cmI("c", "red")): cm("c", "red")},
				result: &kube.Result{Updated: kube.ResourceList{storedInfo("c", "blue")}}}},
			get:  noGet,
			want: []string{"c:specChanged:patch"},
		},
		{
			name: "under Force the verb sent is update",
			writes: []kubeWrite{{op: opUpdate, force: true,
				original: kube.ResourceList{cmI("c", "red")}, target: kube.ResourceList{storedInfo("c", "blue")},
				live:   map[string]runtime.Object{infoKey(cmI("c", "red")): cm("c", "red")},
				result: &kube.Result{Updated: kube.ResourceList{storedInfo("c", "blue")}}}},
			get:  noGet,
			want: []string{"c:specChanged:update"},
		},
		{
			name: "an update that changes only metadata is metadataOnly",
			writes: []kubeWrite{{op: opUpdate,
				original: kube.ResourceList{cmI("m", "red")},
				target:   kube.ResourceList{vinfo(stored(withLabels(cm("m", "red"), map[string]string{"x": "1"})), "configmaps")},
				live:     map[string]runtime.Object{infoKey(cmI("m", "red")): cm("m", "red")},
				result:   &kube.Result{Updated: kube.ResourceList{vinfo(stored(withLabels(cm("m", "red"), map[string]string{"x": "1"})), "configmaps")}}}},
			get:  noGet,
			want: []string{"m:metadataOnly:patch"},
		},
		{
			// An update helm made for an object the old release named and the new one does not, and
			// that it did not delete, is reported through the shared pendingDeletions, as in Reconcile.
			name: "an update's dropped child that helm did not delete is deleteNotDone",
			writes: []kubeWrite{{op: opUpdate,
				original: kube.ResourceList{cmI("gone", "red")}, target: kube.ResourceList{},
				result: &kube.Result{}}},
			get:  func(info *resource.Info) (runtime.Object, error) { return info.Object, nil },
			want: []string{"gone:deleteNotDone:delete"},
		},
		{
			name: "a delete: deleted, not done (still live), unknown (unreadable), already gone (absent)",
			writes: []kubeWrite{{op: opDelete,
				resources: kube.ResourceList{cmI("x", "r"), cmI("y", "r"), cmI("z", "r"), cmI("w", "r")},
				result:    &kube.Result{Deleted: kube.ResourceList{cmI("x", "r")}}}},
			get: func(info *resource.Info) (runtime.Object, error) {
				switch info.Name {
				case "y":
					return info.Object, nil
				case "z":
					return nil, notFoundErr("z")
				default:
					return nil, errors.New("forbidden")
				}
			},
			want: []string{"w:deleteUnknown:delete", "x:deleted:delete", "y:deleteNotDone:delete"},
		},
		{
			name: "hook writes are excluded and counted",
			writes: []kubeWrite{
				{op: opCreate, hook: true, resources: kube.ResourceList{hookInfo("h1")}},
				{op: opDelete, hook: true, resources: kube.ResourceList{hookInfo("h1"), hookInfo("h2")}},
				{op: opCreate, resources: kube.ResourceList{storedInfo("a", "red")}},
			},
			get:       noGet,
			want:      []string{"a:created:create"},
			wantHooks: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, complete, notAtt, hooks := operationVerdicts(tc.writes, content.Keys{}, tc.get)
			if g := changes(got); !equalStrings(g, tc.want) {
				t.Errorf("verdicts = %v, want %v", g, tc.want)
			}
			if !complete {
				t.Error("every write was observed, so the verdicts must be complete")
			}
			if hooks != tc.wantHooks || notAtt != tc.wantNotAtt {
				t.Errorf("hooks=%d notAttempted=%d, want %d and %d", hooks, notAtt, tc.wantHooks, tc.wantNotAtt)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An uninstall's kept children are the release's children helm never sent a delete for: it
// filtered them out itself (helm.sh/resource-policy: keep). A hook delete does not count as asked.
func TestKeptByUninstall(t *testing.T) {
	manifest := kube.ResourceList{cmI("a", "r"), cmI("b", "r"), cmI("keep", "r")}
	writes := []kubeWrite{
		{op: opDelete, resources: kube.ResourceList{cmI("a", "r"), cmI("b", "r")}},
		{op: opDelete, hook: true, resources: kube.ResourceList{cmI("keep", "r")}},
	}
	if got := changes(keptByUninstall(manifest, writes)); !equalStrings(got, []string{"keep:kept:delete"}) {
		t.Errorf("kept = %v, want [keep:kept:delete]", got)
	}
	// Control: when helm asked to delete everything, nothing is kept.
	all := []kubeWrite{{op: opDelete, resources: manifest}}
	if got := keptByUninstall(manifest, all); len(got) != 0 {
		t.Errorf("nothing was filtered, but kept = %v", changes(got))
	}
}

// The recorder must be a faithful KubeClient: every interface helm's actions type-assert for still
// resolves through it (install.go:460 asserts InterfaceThreeWayMerge UNCHECKED), each call reaches
// the real client, and each write is recorded with its kind and hook flag.
func TestKubeRecorderIsAFaithfulKubeClient(t *testing.T) {
	var kc kube.Interface = newKubeRecorder(fakeKube())
	for name, ok := range map[string]bool{
		"InterfaceExt":                 is[kube.InterfaceExt](kc),
		"InterfaceThreeWayMerge":       is[kube.InterfaceThreeWayMerge](kc),
		"InterfaceLogs":                is[kube.InterfaceLogs](kc),
		"InterfaceDeletionPropagation": is[kube.InterfaceDeletionPropagation](kc),
		"InterfaceResources":           is[kube.InterfaceResources](kc),
	} {
		if !ok {
			t.Errorf("the recorder does not satisfy kube.%s, which helm's actions assert for", name)
		}
	}
	r := kc.(*kubeRecorder)
	if _, err := r.Create(kube.ResourceList{cmI("a", "r")}); err != nil {
		t.Fatalf("Create through the recorder: %v", err)
	}
	if _, errs := r.Delete(kube.ResourceList{hookInfo("h")}); len(errs) != 0 {
		t.Fatalf("Delete through the recorder: %v", errs)
	}
	w := r.recorded()
	if len(w) != 2 || w[0].op != opCreate || w[0].hook || w[1].op != opDelete || !w[1].hook {
		t.Errorf("recorded %+v, want a non-hook create then a hook delete", w)
	}
}

// fakeKube is helm's FailingKubeClient with no failures: the fake that implements every optional
// interface fullKube needs (PrintingKubeClient alone lacks UpdateThreeWayMerge).
func fakeKube() *kubefake.FailingKubeClient {
	return &kubefake.FailingKubeClient{PrintingKubeClient: kubefake.PrintingKubeClient{Out: discard{}}}
}

func is[T any](v any) bool { _, ok := v.(T); return ok }

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// A configuration whose KubeClient cannot be wrapped makes the result explicitly incomplete, never
// a silently short list.
func TestWriteObserverMarksAnUnwrappableClientIncomplete(t *testing.T) {
	var o writeObserver
	o.attach(&action.Configuration{KubeClient: minimalKube{}})
	if r := o.result(nil, content.Keys{}, nil); r.ObjectsComplete {
		t.Error("an unobserved KubeClient must leave ObjectsComplete false")
	}
	// Control: a full client is wrapped and the (empty) result is complete.
	var full writeObserver
	ac := &action.Configuration{KubeClient: fakeKube()}
	full.attach(ac)
	if _, ok := ac.KubeClient.(*kubeRecorder); !ok {
		t.Fatal("a full KubeClient must be wrapped in a recorder")
	}
	if r := full.result(nil, content.Keys{}, nil); !r.ObjectsComplete {
		t.Error("a wrapped client with no writes is complete")
	}
}

// minimalKube implements only kube.Interface, so it cannot be wrapped.
type minimalKube struct{ kube.Interface }
