package helm

import (
	"encoding/json"
	"fmt"

	"helm.sh/helm/v3/pkg/kube"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/cli-runtime/pkg/resource"
)

// Skipping the objects whose own patch is empty (core-provider#697).
//
// Reconcile's step 4 reads every target child. helm's UpdateThreeWayMerge then reads each one three
// more times (pkg/kube/client.go in v3.20.2: the existence check in update, the live fetch inside
// createPatch, and the refresh after an empty patch) whether or not anything drifted. At steady
// state that is four of the six GETs per child per pass, for no write.
//
// The decision to leave a child out of the merge is helm's own: the patch helm's update would send
// for it, computed from the live object step 4 already holds. An empty patch is exactly the case in
// which helm's update sends nothing ("Looks like there are no changes").

// patchIsEmpty reports whether helm's three-way patch for target is empty: the patch that
// UpdateThreeWayMerge's createPatch (unexported) builds from the same three documents, original (the
// stored manifest's object), target (info.Object) and live. createPatch cannot be called from here,
// so this calls the pieces it is made of, all exported: kube.AsVersioned to choose the patch type,
// then jsonmergepatch.CreateThreeWayJSONMergePatch (with createPatch's preconditions) for
// unstructured objects and CRDs, strategicpatch.CreateThreeWayMergePatch for built-in kinds.
//
// An error means undetermined, and the caller keeps the object in the merge, where helm reports the
// same error itself.
//
// TestEnvtest_SkipDecisionAgreesWithHelm holds this to helm's behaviour: an object is patched by
// helm's update exactly when this returns false.
func patchIsEmpty(target *resource.Info, original, live runtime.Object) (bool, error) {
	oldData, err := json.Marshal(original)
	if err != nil {
		return false, fmt.Errorf("serializing current configuration: %w", err)
	}
	newData, err := json.Marshal(target.Object)
	if err != nil {
		return false, fmt.Errorf("serializing target configuration: %w", err)
	}
	liveData, err := json.Marshal(live)
	if err != nil {
		return false, fmt.Errorf("serializing live configuration: %w", err)
	}

	versioned := kube.AsVersioned(target)
	_, isUnstructured := versioned.(runtime.Unstructured)
	_, isCRD := versioned.(*apiextv1beta1.CustomResourceDefinition)

	var patch []byte
	if isUnstructured || isCRD {
		preconditions := []mergepatch.PreconditionFunc{
			mergepatch.RequireKeyUnchanged("apiVersion"),
			mergepatch.RequireKeyUnchanged("kind"),
			mergepatch.RequireMetadataKeyUnchanged("name"),
		}
		patch, err = jsonmergepatch.CreateThreeWayJSONMergePatch(oldData, newData, liveData, preconditions...)
	} else {
		var meta strategicpatch.LookupPatchMeta
		meta, err = strategicpatch.NewPatchMetaFromStruct(versioned)
		if err != nil {
			return false, fmt.Errorf("creating patch metadata: %w", err)
		}
		patch, err = strategicpatch.CreateThreeWayMergePatch(oldData, newData, liveData, meta, true)
	}
	if err != nil {
		return false, err
	}
	return patch == nil || string(patch) == "{}", nil
}

// undriftedTargets returns the infos of target that the merge may leave out: those that exist live
// (absent), are in current, were not adopted this pass, and whose patch is empty. Everything else
// goes to the merge: creations, adoptions, drifted objects, and any object whose patch could not be
// computed.
//
// live is each target's state from step 4 and target is read AFTER steps 4.5 to 4.7, as the merge
// receives it.
func undriftedTargets(current, target kube.ResourceList, live map[string]runtime.Object, absent, adopted map[string]bool) kube.ResourceList {
	var out kube.ResourceList
	for _, info := range target {
		key := infoKey(info)
		if absent[key] || adopted[key] || live[key] == nil {
			continue
		}
		orig := current.Get(info)
		if orig == nil {
			continue
		}
		if empty, err := patchIsEmpty(info, orig.Object, live[key]); err == nil && empty {
			out = append(out, info)
		}
	}
	return out
}

// withoutMatching returns list without every entry helm matches (group, kind, name and namespace,
// whatever the version) to one of drop. Matching by helm's own rule rather than by infoKey: a stored
// entry at an older apiVersion has another key than its target, and leaving it in current while its
// target left the merge would read to helm as a deletion. EVERY match goes, not the first: a list
// that names one object twice would otherwise keep the second, and helm would delete it.
func withoutMatching(list, drop kube.ResourceList) kube.ResourceList {
	if len(drop) == 0 {
		return list
	}
	return list.Filter(func(info *resource.Info) bool { return !drop.Contains(info) })
}
