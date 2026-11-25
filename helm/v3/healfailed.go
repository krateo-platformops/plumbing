package helm

import (
	"fmt"
	"strings"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/kube"
)

// healFailedRelease is the heal Reconcile runs before retrying the upgrade of a release whose latest
// revision failed (core-provider#640; see helmconfig.FailedReleaseHeal). stored is that revision as
// Reconcile read it.
//
// It returns the heal's report and the verdicts of its writes. An error is the heal's own
// (heal.Err); a release with no record is not an error.
func (c *client) healFailedRelease(ac *action.Configuration, releaseName string, stored *helmconfig.Release, cfg *helmconfig.UpgradeConfig) (*helmconfig.FailedReleaseHeal, []helmconfig.ObjectVerdict, bool) {
	heal := &helmconfig.FailedReleaseHeal{Revision: stored.Revision}

	// The record is read from storage, with its labels: the wrapper Release Reconcile holds has none.
	last, err := ac.Releases.Last(releaseName)
	if err != nil {
		heal.Err = fmt.Errorf("reading revision %d: %w", stored.Revision, err)
		return heal, nil, true
	}
	if last.Version != stored.Revision {
		// Another writer added a revision after Reconcile read the release: what it records is not
		// what Reconcile decided on, so nothing is healed this pass. Said as such, not as "no record".
		heal.Err = fmt.Errorf("revision %d is no longer the latest: revision %d was written since", stored.Revision, last.Version)
		return heal, nil, true
	}
	if !hasAppliedRecord(last) {
		return heal, nil, true // no record: nothing to heal against, and not an error
	}
	heal.Recorded = true

	mapper, err := ac.RESTClientGetter.ToRESTMapper()
	if err != nil {
		heal.Err = fmt.Errorf("rest mapper: %w", err)
		return heal, nil, true
	}
	manifest, dropped := filterUnmappableManifestDocs(last.Manifest, mapper)
	if len(dropped) > 0 && c.debugLog != nil {
		c.debugLog("reconcile: healing %s: revision %d records kinds the cluster no longer serves, not healed: %s",
			releaseName, last.Version, strings.Join(dropped, ", "))
	}
	kc := ac.KubeClient
	tw, ok := kc.(kube.InterfaceThreeWayMerge)
	if !ok {
		heal.Err = fmt.Errorf("the kube client %T cannot merge", kc)
		return heal, nil, true
	}
	// The record is both sides of the merge, as one list: helm patches each object against its
	// original before it refreshes it, and there is nothing to delete.
	target, err := kc.Build(strings.NewReader(manifest), false)
	if err != nil {
		heal.Err = fmt.Errorf("building revision %d's record: %w", last.Version, err)
		return heal, nil, true
	}

	liveObj, absent := readLive(target)
	// As Reconcile's 4.5 and 4.6: the trace-context stamps a child carries are not drift, and a
	// Secret's stringData is stored as data. Without these, every pass of a release that keeps
	// failing would patch them again.
	for _, info := range target {
		key := infoKey(info)
		if absent[key] {
			continue
		}
		for _, ann := range volatileTraceAnnotations {
			if v := annotationOf(liveObj[key], ann); v != "" {
				setAnnotation(info.Object, ann, v)
			}
		}
	}
	for _, info := range target {
		normalizeSecretStringData(info.Object)
	}
	// As Reconcile's 5a: a live Job is left alone. Its pod template is immutable and it runs once.
	target = withoutKeys(target, classifyStoredJobs(target, target, liveObj).skip)
	current := target

	owned := ownedFieldMasks(current, target)
	res, err := tw.UpdateThreeWayMerge(current, target, cfg.Force)
	applyErr, abort := classifyApply(res, err)
	if abort {
		heal.Err = fmt.Errorf("healing against revision %d: %w", last.Version, applyErr)
		return heal, nil, false
	}
	judged := judgeMerge(res, current, target, liveObj, absent, owned, cfg)
	heal.Created, heal.PatchedUpdated = judged.Created, judged.PatchedUpdated
	heal.ChangedObjects, heal.ChangedObjectsOmitted = judged.ChangedObjects, judged.ChangedObjectsOmitted
	heal.ChangedPaths, heal.ChangedPathsOmitted = judged.ChangedPaths, judged.ChangedPathsOmitted
	if applyErr != nil {
		heal.Err = fmt.Errorf("healing against revision %d: %w", last.Version, applyErr)
	}
	return heal, judged.Objects, judged.ObjectsComplete
}
