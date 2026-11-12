package helm

import (
	"sort"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/kubeutil/content"
	"helm.sh/helm/v3/pkg/kube"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
)

// verdictInputs is what the self-heal merge left behind, as Reconcile holds it after step 5.
//
// target holds the children handed to the merge, each Object refreshed to its post-merge state;
// live holds their state before the merge (absent ones are missing from it). created, updated and
// deleted are helm's result.
//
// helm's result alone can't tell a failed write from a successful one: it appends a child to
// created BEFORE sending the create ("even if something fails") and every applied child to updated
// "regardless" of its patch's outcome (helm v3 pkg/kube/client.go, update). What does tell them
// apart is the object: helm refreshes it from the server (Refresh after a create or patch, Get
// after an empty patch) only when that succeeded. A rendered object never carries a
// resourceVersion, so a child of created or updated still without one is a failed write. kept, notDone and unknown are the infoKeys of intended deletions that helm
// declined (resource-policy keep), did not perform, or whose state could not be read
// (pendingDeletions), and current resolves them to their Infos.
type verdictInputs struct {
	target, created, updated, deleted, current kube.ResourceList
	live                                       map[string]runtime.Object
	absent                                     map[string]bool
	kept, notDone, unknown                     []string
	force                                      bool
	// contentAnnotations are the caller's UpgradeConfig.ContentAnnotations.
	contentAnnotations []string
}

// objectVerdicts builds the uncapped per-child verdict list (plumbing#106).
//
// A surviving child whose write helm confirmed is listed exactly when Reconcile counts it under
// PatchedUpdated: its stripped state changed across the merge, or the comparison failed. Both use
// writeConfirmed, so the verdicts and the counts agree by construction, a partial apply included:
// Created, Deleted and PatchedUpdated equal the created, deleted and other surviving-child
// verdicts other than the failed ones (createFailed, updateFailed), which no count includes.
//
// EXTRACTED for the same reason as classifyApply and keptDeletions: a decision reachable only
// through a full Reconcile is a decision nobody tests.
//
// complete is false when an intended deletion's key resolves to no Info in current, which the
// lists it came from should make impossible; the verdict is then missing, and the caller is told
// rather than left to read the gap as "nothing happened".
//
// notAttempted counts the targets helm never reached (in neither created nor updated).
func objectVerdicts(in verdictInputs) (verdicts []helmconfig.ObjectVerdict, complete bool, notAttempted int) {
	updateVerb := "patch"
	if in.force {
		updateVerb = "update" // helm replaces (PUT) under Force
	}
	var out []helmconfig.ObjectVerdict
	for _, info := range in.created {
		if !writeConfirmed(info) {
			out = append(out, verdictOf(info, info.Object, "create", helmconfig.ChangeCreateFailed))
			continue
		}
		out = append(out, verdictOf(info, info.Object, "create", helmconfig.ChangeCreated))
	}
	createdKeys := make(map[string]bool, len(in.created))
	for _, info := range in.created {
		createdKeys[infoKey(info)] = true
	}
	applied := make(map[string]bool, len(in.updated))
	for _, info := range in.updated {
		applied[infoKey(info)] = true
	}
	for _, info := range in.deleted {
		out = append(out, verdictOf(info, info.Object, "delete", helmconfig.ChangeDeleted))
	}
	for _, info := range in.target {
		key := infoKey(info)
		if in.absent[key] {
			if !createdKeys[key] {
				notAttempted++
			}
			continue // listed from created
		}
		before := in.live[key]
		if !writeConfirmed(info) {
			if applied[key] {
				// The uid is the live object's: the failed write left this one as rendered.
				out = append(out, verdictOf(info, before, updateVerb, helmconfig.ChangeUpdateFailed))
			} else {
				// Not applied: helm stopped before reaching it, so nothing was sent.
				notAttempted++
			}
			continue
		}
		changed, err := semanticallyChanged(before, info.Object)
		if err != nil {
			out = append(out, verdictOf(info, info.Object, updateVerb, helmconfig.ChangeCompareFailed))
			continue
		}
		if !changed {
			continue
		}
		contentChanged, err := content.Differs(before, info.Object, in.contentAnnotations)
		switch {
		case err != nil:
			out = append(out, verdictOf(info, info.Object, updateVerb, helmconfig.ChangeCompareFailed))
		case contentChanged:
			out = append(out, verdictOf(info, info.Object, updateVerb, helmconfig.ChangeSpec))
		default:
			out = append(out, verdictOf(info, info.Object, updateVerb, helmconfig.ChangeMetadataOnly))
		}
	}
	byKey := make(map[string]*resource.Info, len(in.current))
	for _, info := range in.current {
		byKey[infoKey(info)] = info
	}
	complete = true
	for _, d := range []struct {
		keys   []string
		change helmconfig.ObjectChange
	}{
		{in.kept, helmconfig.ChangeKept},
		{in.notDone, helmconfig.ChangeDeleteNotDone},
		{in.unknown, helmconfig.ChangeDeleteUnknown},
	} {
		for _, k := range d.keys {
			info := byKey[k]
			if info == nil {
				complete = false
				continue
			}
			out = append(out, verdictOf(info, info.Object, "delete", d.change))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return verdictKey(out[i]) < verdictKey(out[j]) })
	return out, complete, notAttempted
}

func verdictKey(v helmconfig.ObjectVerdict) string {
	return v.Group + "/" + v.Version + "/" + v.Resource + "/" + v.Namespace + "/" + v.Name + "/" + string(v.Change)
}

// verdictOf names info's child. The resource comes from the REST mapping helm used to send the
// request; the uid from obj when it carries one.
func verdictOf(info *resource.Info, obj runtime.Object, verb string, change helmconfig.ObjectChange) helmconfig.ObjectVerdict {
	var gvk schema.GroupVersionKind
	if obj != nil {
		gvk = obj.GetObjectKind().GroupVersionKind()
	}
	v := helmconfig.ObjectVerdict{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind,
		Namespace: info.Namespace, Name: info.Name, Verb: verb, Change: change,
	}
	if info.Mapping != nil {
		v.Group, v.Version, v.Resource = info.Mapping.Resource.Group, info.Mapping.Resource.Version, info.Mapping.Resource.Resource
		if v.Kind == "" {
			v.Kind = info.Mapping.GroupVersionKind.Kind
		}
	}
	if obj != nil {
		if acc, err := apimeta.Accessor(obj); err == nil {
			v.UID = string(acc.GetUID())
		}
	}
	return v
}
