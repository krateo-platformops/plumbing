package helm

import (
	"sort"
	"sync"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/action"

	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/resource"
)

// fullKube is every interface helm's actions use on action.Configuration.KubeClient: the base
// Interface, and the optional ones they type-assert for (helm v3.20.2):
//   - InterfaceThreeWayMerge, asserted UNCHECKED at install.go:460, so a client without it panics;
//   - InterfaceExt (hooks.go:165, uninstall.go:135);
//   - InterfaceLogs (hooks.go:217);
//   - InterfaceDeletionPropagation (uninstall.go:240);
//   - InterfaceResources (status.go:71).
//
// *kube.Client implements all of them, and so does helm's kube/fake.FailingKubeClient (its
// PrintingKubeClient alone lacks UpdateThreeWayMerge).
// kubeRecorder embeds a fullKube, so every method it does not override is helm's own, and every
// type assertion an action makes on the configuration succeeds exactly as it would unwrapped.
type fullKube interface {
	kube.Interface
	kube.InterfaceExt
	kube.InterfaceThreeWayMerge
	kube.InterfaceLogs
	kube.InterfaceDeletionPropagation
	kube.InterfaceResources
}

var _ fullKube = (*kube.Client)(nil)

// kubeOp is the kind of write a kubeWrite recorded.
type kubeOp string

const (
	opCreate kubeOp = "create"
	opUpdate kubeOp = "update"
	opDelete kubeOp = "delete"
)

// kubeWrite is one write helm sent through the KubeClient, as sent and as answered.
type kubeWrite struct {
	op kubeOp
	// hook: every object in the call carries helm.sh/hook, so it is a hook write, not a write to a
	// release child. helm writes hooks in their own calls (hooks.go), never mixed with children.
	hook bool
	// resources is the Create or Delete input; for an Update, original is helm's current list and
	// target the desired one.
	resources, original, target kube.ResourceList
	// live and absent are, for an Update, each target's state just BEFORE the call, keyed by
	// infoKey: the "before" the verdict compares against, as Reconcile's step 4 reads it.
	live   map[string]runtime.Object
	absent map[string]bool
	force  bool
	result *kube.Result
	err    error
	errs   []error
}

// kubeRecorder is the KubeClient plumbing hands helm's actions: it forwards every call to the real
// client and records each write. One recorder per action configuration, so per attempt of
// retryOnStaleMapping, and what it records is only that attempt's writes.
type kubeRecorder struct {
	fullKube
	mu     sync.Mutex
	writes []kubeWrite
}

func newKubeRecorder(inner fullKube) *kubeRecorder { return &kubeRecorder{fullKube: inner} }

func (r *kubeRecorder) record(w kubeWrite) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, w)
}

// recorded is a copy of the writes so far.
func (r *kubeRecorder) recorded() []kubeWrite {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]kubeWrite(nil), r.writes...)
}

func (r *kubeRecorder) Create(resources kube.ResourceList) (*kube.Result, error) {
	res, err := r.fullKube.Create(resources)
	r.record(kubeWrite{op: opCreate, hook: allHooks(resources), resources: resources, result: res, err: err})
	return res, err
}

func (r *kubeRecorder) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	live, absent := readLive(target)
	res, err := r.fullKube.Update(original, target, force)
	r.record(kubeWrite{op: opUpdate, hook: allHooks(target), original: original, target: target, live: live, absent: absent, force: force, result: res, err: err})
	return res, err
}

func (r *kubeRecorder) UpdateThreeWayMerge(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	live, absent := readLive(target)
	res, err := r.fullKube.UpdateThreeWayMerge(original, target, force)
	r.record(kubeWrite{op: opUpdate, hook: allHooks(target), original: original, target: target, live: live, absent: absent, force: force, result: res, err: err})
	return res, err
}

func (r *kubeRecorder) Delete(resources kube.ResourceList) (*kube.Result, []error) {
	res, errs := r.fullKube.Delete(resources)
	r.record(kubeWrite{op: opDelete, hook: allHooks(resources), resources: resources, result: res, errs: errs})
	return res, errs
}

func (r *kubeRecorder) DeleteWithPropagationPolicy(resources kube.ResourceList, policy metav1.DeletionPropagation) (*kube.Result, []error) {
	res, errs := r.fullKube.DeleteWithPropagationPolicy(resources, policy)
	r.record(kubeWrite{op: opDelete, hook: allHooks(resources), resources: resources, result: res, errs: errs})
	return res, errs
}

// allHooks reports whether every object in l is a hook. An empty list is not a hook write.
func allHooks(l kube.ResourceList) bool {
	if len(l) == 0 {
		return false
	}
	for _, info := range l {
		if annotationOf(info.Object, release.HookAnnotation) == "" {
			return false
		}
	}
	return true
}

// readLive reads each target's live state with a GET that leaves the info untouched, as
// Reconcile's step 4 does. A failed GET (typically NotFound) marks the child absent: helm's update
// will create it, and the create's own outcome is in the result.
func readLive(target kube.ResourceList) (map[string]runtime.Object, map[string]bool) {
	live := make(map[string]runtime.Object, len(target))
	absent := make(map[string]bool, len(target))
	for _, info := range target {
		if info == nil || info.Mapping == nil || info.Client == nil {
			continue
		}
		obj, err := resource.NewHelper(info.Client, info.Mapping).Get(info.Namespace, info.Name)
		if err != nil {
			absent[infoKey(info)] = true
			continue
		}
		live[infoKey(info)] = obj
	}
	return live, absent
}

// writeObserver collects the writes of every attempt of one operation (retryOnStaleMapping may run
// it twice, and a write an earlier attempt made is still a write) and turns them into the
// operation's OperationResult.
type writeObserver struct {
	recs []*kubeRecorder
	// unobserved: an attempt's KubeClient could not be wrapped, so its writes are unknown.
	unobserved bool
	// unresolved: an uninstalled release's manifest could not be built, so its kept children
	// cannot be named.
	unresolved bool
}

// attach wraps ac's KubeClient in a recorder for this attempt.
func (o *writeObserver) attach(ac *action.Configuration) {
	inner, ok := ac.KubeClient.(fullKube)
	if !ok {
		o.unobserved = true
		return
	}
	r := newKubeRecorder(inner)
	ac.KubeClient = r
	o.recs = append(o.recs, r)
}

// result builds the OperationResult. manifest is an uninstalled release's children, for the kept
// verdicts; nil for any other operation.
func (o *writeObserver) result(rel *helmconfig.Release, contentAnnotations []string, manifest kube.ResourceList) *helmconfig.OperationResult {
	var writes []kubeWrite
	for _, r := range o.recs {
		writes = append(writes, r.recorded()...)
	}
	objs, complete, notAttempted, hooks := operationVerdicts(writes, contentAnnotations, getLive)
	if manifest != nil {
		objs = append(objs, keptByUninstall(manifest, writes)...)
		sort.SliceStable(objs, func(i, j int) bool { return verdictKey(objs[i]) < verdictKey(objs[j]) })
	}
	return &helmconfig.OperationResult{
		Release:             rel,
		Objects:             objs,
		ObjectsComplete:     complete && !o.unobserved && !o.unresolved,
		ObjectsNotAttempted: notAttempted,
		HooksExcluded:       hooks,
	}
}
