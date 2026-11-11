package helm

import (
	"sort"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/resource"
)

// operationVerdicts turns the writes one action sent through a kubeRecorder into per-child verdicts
// (plumbing#135), with the same classification Reconcile uses (objectVerdicts), so every write
// site reads alike to a caller writing audit records.
//
//   - a hook write is counted in hooks and left out of the verdicts;
//   - a Create: each object helm refreshed from the server is created, the rest createFailed
//     (helm's perform attempts every object, so an unrefreshed one failed, it was not skipped);
//   - an Update (Upgrade, Rollback, an Install's adoption): objectVerdicts over helm's result, the
//     live state read just before the call, and the deletions helm did not perform
//     (pendingDeletions), exactly as Reconcile builds its own;
//   - a Delete: each object in helm's result is deleted; one helm was asked to delete and did not is
//     deleteNotDone, or deleteUnknown when its live state cannot be read; one already gone is in
//     neither.
//
// get reads a child's live state (getLive in production, refreshing the info).
func operationVerdicts(writes []kubeWrite, contentAnnotations []string, get func(*resource.Info) (runtime.Object, error)) (out []helmconfig.ObjectVerdict, complete bool, notAttempted, hooks int) {
	complete = true
	for _, w := range writes {
		if w.hook {
			hooks += len(w.resources) + len(w.target)
			continue
		}
		res := w.result
		if res == nil {
			res = &kube.Result{}
		}
		switch w.op {
		case opCreate:
			v, _, _ := objectVerdicts(verdictInputs{created: w.resources})
			out = append(out, v...)
		case opUpdate:
			kept, notDone, unknown := pendingDeletions(w.original, w.target, res.Deleted, get)
			v, c, na := objectVerdicts(verdictInputs{
				target: w.target, created: res.Created, updated: res.Updated, deleted: res.Deleted,
				current: w.original, live: w.live, absent: w.absent,
				kept: kept, notDone: notDone, unknown: unknown,
				force: w.force, contentAnnotations: contentAnnotations,
			})
			out = append(out, v...)
			complete = complete && c
			notAttempted += na
		case opDelete:
			out = append(out, deleteVerdicts(w.resources, res.Deleted, get)...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return verdictKey(out[i]) < verdictKey(out[j]) })
	return out, complete, notAttempted, hooks
}

// deleteVerdicts classifies one Delete call: what helm reports deleted is deleted; what it was asked
// to delete and did not is deleteNotDone (still live) or deleteUnknown (unreadable); what is
// already gone is in neither.
func deleteVerdicts(asked, deleted kube.ResourceList, get func(*resource.Info) (runtime.Object, error)) []helmconfig.ObjectVerdict {
	var out []helmconfig.ObjectVerdict
	gone := make(map[string]bool, len(deleted))
	for _, info := range deleted {
		gone[infoKey(info)] = true
		out = append(out, verdictOf(info, info.Object, "delete", helmconfig.ChangeDeleted))
	}
	for _, info := range asked {
		if gone[infoKey(info)] {
			continue
		}
		obj, err := get(info)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil || obj == nil:
			out = append(out, verdictOf(info, info.Object, "delete", helmconfig.ChangeDeleteUnknown))
		default:
			out = append(out, verdictOf(info, obj, "delete", helmconfig.ChangeDeleteNotDone))
		}
	}
	return out
}

// keptByUninstall names the release children helm's uninstall did NOT ask to delete: the release
// manifest's objects minus every non-hook Delete input. helm filters those out itself
// (helm.sh/resource-policy: keep, read from the manifest by its unexported filterManifestsToKeep),
// so this observes helm's decision rather than re-implementing it. Each is a ChangeKept verdict,
// with the verb the release asked for.
func keptByUninstall(manifest kube.ResourceList, writes []kubeWrite) []helmconfig.ObjectVerdict {
	asked := map[string]bool{}
	for _, w := range writes {
		if w.op == opDelete && !w.hook {
			for _, info := range w.resources {
				asked[infoKey(info)] = true
			}
		}
	}
	var out []helmconfig.ObjectVerdict
	for _, info := range manifest {
		if !asked[infoKey(info)] {
			out = append(out, verdictOf(info, info.Object, "delete", helmconfig.ChangeKept))
		}
	}
	return out
}
