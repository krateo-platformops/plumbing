package helm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/cli-runtime/pkg/resource"
)

// AppliedManifestLabel marks a revision whose stored manifest is what was APPLIED to the cluster,
// not what was rendered (core-provider#640). helm stores a failed revision with its render, whether
// or not the render reached the cluster, so on its own a failed revision says nothing about which
// objects exist and in what form. plumbing's upgrades and failed-install retries store what they
// applied instead, and say so with this label on the release record (on the release Secret, beside
// helm's own name, owner, status and version labels).
//
// Its value is appliedDigest of the revision: the release name, the revision number and the stored
// manifest. A label helm copies onto a later revision (it carries custom labels forward), one left
// on a revision whose manifest something else rewrote, or one on a rollback, does not match, so it
// never marks a manifest plumbing did not record.
const AppliedManifestLabel = "plumbing.krateo.io/applied-manifest"

// appliedDigest is the value AppliedManifestLabel carries for revision version of release name when
// it stores manifest: "sha256-" and the first 32 hex digits, within a label value's 63 characters.
func appliedDigest(name string, version int, manifest string) string {
	sum := sha256.Sum256([]byte(name + "\n" + strconv.Itoa(version) + "\n" + manifest))
	return "sha256-" + hex.EncodeToString(sum[:16])
}

// hasAppliedRecord reports whether rel's manifest is a record of what was applied: the label is
// there and matches it.
func hasAppliedRecord(rel *release.Release) bool {
	if rel == nil || rel.Labels == nil {
		return false
	}
	return rel.Labels[AppliedManifestLabel] == appliedDigest(rel.Name, rel.Version, rel.Manifest)
}

// markApplied sets or removes rel's AppliedManifestLabel for its current manifest. It copies the
// label map: helm shares it with the in-memory release it carries on.
func markApplied(rel *release.Release, applied bool) {
	labels := make(map[string]string, len(rel.Labels)+1)
	for k, v := range rel.Labels {
		labels[k] = v
	}
	if applied {
		labels[AppliedManifestLabel] = appliedDigest(rel.Name, rel.Version, rel.Manifest)
	} else {
		delete(labels, AppliedManifestLabel)
	}
	rel.Labels = labels
}

// appliedBaseline is what is applied on the cluster when an operation follows last, as far as the
// release records can prove it. A deployed revision's manifest was applied: helm stores a revision
// as deployed only after its objects step and its hooks succeeded. A failed one's was, only when it
// carries a valid AppliedManifestLabel. Anything else (a failed revision written by helm alone, or
// by a plumbing release before this record existed) is unknown, and known is false.
func appliedBaseline(last *release.Release) (manifest string, known bool) {
	if last == nil || last.Info == nil {
		return "", false
	}
	switch {
	case last.Info.Status == release.StatusDeployed:
		return last.Manifest, true
	case last.Info.Status == release.StatusFailed && hasAppliedRecord(last):
		return last.Manifest, true
	}
	return "", false
}

// upgradeRecord is one attempt of Client.Upgrade, made to store what it applied (core-provider#640),
// as failedRetry does for a retried first install.
//
// helm stores the revision with its render, from the moment it creates it. This attempt's revision
// stores instead:
//
//   - until the objects step: the baseline, what is applied (appliedBaseline). An attempt that fails
//     in a pre-upgrade hook applied nothing, and its revision says so.
//   - after an objects step helm reports as done: the render, plus each dropped object helm did not
//     delete (recordSuccess).
//   - after one that failed: each object's last confirmed document (recordPartial).
//
// The revision is marked (AppliedManifestLabel) only when that is a fact: always after a successful
// objects step, and otherwise only when the baseline was known.
//
// When the latest revision is FAILED and recorded, the objects step merges from that record instead
// of from the last deployed revision, which is what helm merges from. Objects only a failed attempt
// applied are then deleted when the render drops them (plumbing#232), as a retried first install
// deletes them. Every other upgrade's objects step is helm's own.
type upgradeRecord struct {
	releaseName, namespace string
	kube                   fullKube
	driver                 driver.Driver
	debugLog               action.DebugLog

	// last is the latest revision when the attempt started; this attempt's is last.Version+1.
	last *release.Release
	// baseline is what is applied (appliedBaseline), and known whether it is a fact.
	baseline string
	known    bool
	// substitute: the objects step merges from baseline (built as baselineManifest, without the kinds
	// the cluster no longer serves) instead of from helm's original.
	substitute       bool
	baselineManifest string

	mu            sync.Mutex
	key           string // this attempt's storage key; empty until helm created its revision
	rendered      string // this attempt's render
	recorded      string // the manifest this attempt's revision stores
	recordedKnown bool   // recorded is what is applied
	applied       bool   // the objects step ran
}

// prepareUpgradeRecord installs the record's seams on ac, for the upgrade of releaseName that is about
// to run on it. It writes nothing. A release with no revisions, or whose latest is pending, gets no
// seams: helm refuses to upgrade either.
func (c *client) prepareUpgradeRecord(releaseName, namespace string, ac *action.Configuration) (*upgradeRecord, error) {
	last, err := ac.Releases.Last(releaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the latest revision: %w", err)
	}
	if last.Info == nil || last.Info.Status.IsPending() {
		return nil, nil
	}
	inner, ok := ac.KubeClient.(fullKube)
	if !ok {
		return nil, nil // not helm's client (a test fake): helm's own record
	}
	baseline, known := appliedBaseline(last)
	r := &upgradeRecord{
		releaseName: releaseName, namespace: namespace,
		kube: inner, driver: ac.Releases.Driver, debugLog: c.debugLog,
		last: last, baseline: baseline, known: known,
		substitute: known && last.Info.Status == release.StatusFailed,
	}
	if r.substitute {
		mapper, err := ac.RESTClientGetter.ToRESTMapper()
		if err != nil {
			return nil, fmt.Errorf("rest mapper: %w", err)
		}
		manifest, dropped := filterUnmappableManifestDocs(baseline, mapper)
		if len(dropped) > 0 && c.cachedClients != nil {
			// As prepareRetryFailed: a kind is dropped only when fresh discovery says it is not served.
			c.cachedClients.mapper.Reset()
			manifest, dropped = filterUnmappableManifestDocs(baseline, mapper)
		}
		if len(dropped) > 0 {
			r.logf("upgrading %s: revision %d names kinds the cluster no longer serves, left out of the merge: %s",
				releaseName, last.Version, strings.Join(dropped, ", "))
		}
		r.baselineManifest = manifest
	}
	ac.KubeClient = &recordKube{fullKube: inner, r: r}
	ac.Releases.Driver = &recordDriver{Driver: ac.Releases.Driver, r: r}
	return r, nil
}

func (r *upgradeRecord) logf(format string, v ...interface{}) {
	if r.debugLog != nil {
		r.debugLog(format, v...)
		return
	}
	slog.Debug(fmt.Sprintf(format, v...))
}

func (r *upgradeRecord) attemptStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.key != ""
}

// apply is the objects step. update is the method helm called, so an upgrade keeps helm's own merge.
func (r *upgradeRecord) apply(original, target kube.ResourceList, force bool, update func(original, target kube.ResourceList, force bool) (*kube.Result, error)) (*kube.Result, error) {
	current := original
	var docs map[string]string
	if r.substitute {
		var err error
		current, docs, err = r.fromBaseline(target)
		if err != nil {
			return nil, err
		}
	} else if r.known {
		built, err := r.kube.Build(strings.NewReader(r.baseline), false)
		if err != nil {
			return nil, fmt.Errorf("building revision %d's manifest: %w", r.last.Version, err)
		}
		if docs, err = documentsOf(built); err != nil {
			return nil, err
		}
	}

	fresh, err := r.renderedDocuments()
	if err != nil {
		return nil, err
	}
	// Before the first write: what is applied, plus what this attempt is about to write. A process
	// killed in the merge leaves this, so nothing it created is unrecorded.
	r.mu.Lock()
	r.recorded, r.recordedKnown = recordIntent(current, target, docs, fresh), r.known
	err = r.persistLocked()
	r.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("recording what is about to be applied: %w", err)
	}

	res, err := update(current, target, force)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = true
	if err == nil {
		r.recorded, r.recordedKnown = recordSuccess(r.rendered, current, target, res, docs), true
	} else {
		r.recorded, r.recordedKnown = recordPartial(current, target, docs, fresh), r.known
	}
	if perr := r.persistLocked(); perr != nil {
		// helm's own record of the outcome follows, through recordDriver.Update, with the same manifest.
		r.logf("upgrading %s: recording what was applied: %v", r.releaseName, perr)
	}
	return res, err
}

// fromBaseline is the original the objects step merges from when the latest revision failed: its
// record, without the dropped objects that are gone or are not this release's, and with each live
// target it does not name adopted as the render. docs is each record object's document.
func (r *upgradeRecord) fromBaseline(target kube.ResourceList) (kube.ResourceList, map[string]string, error) {
	built, err := r.kube.Build(strings.NewReader(r.baselineManifest), false)
	if err != nil {
		return nil, nil, fmt.Errorf("building revision %d's record: %w", r.last.Version, err)
	}
	docs, err := documentsOf(built)
	if err != nil {
		return nil, nil, err
	}
	var current kube.ResourceList
	for _, info := range built {
		if target.Get(info) == nil {
			live, err := resource.NewHelper(info.Client, info.Mapping).Get(info.Namespace, info.Name)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("reading %s, which the render drops: %w", infoKey(info), err)
			}
			if annotationOf(live, helmReleaseNameAnnotation) != r.releaseName || annotationOf(live, helmReleaseNamespaceAnnotation) != r.namespace {
				r.logf("upgrading %s: %s is dropped by the render but is not this release's, left alone", r.releaseName, infoKey(info))
				continue
			}
		}
		current = append(current, info)
	}
	_, absent := readLive(target)
	for _, info := range target {
		if absent[infoKey(info)] || current.Get(info) != nil {
			continue
		}
		adopted := *info
		adopted.Object = info.Object.DeepCopyObject()
		current = append(current, &adopted)
	}
	return current, docs, nil
}

func (r *upgradeRecord) renderedDocuments() (map[string]string, error) {
	r.mu.Lock()
	rendered := r.rendered
	r.mu.Unlock()
	fresh, err := r.kube.Build(strings.NewReader(rendered), false)
	if err != nil {
		return nil, fmt.Errorf("building the rendered manifest: %w", err)
	}
	return documentsOf(fresh)
}

// persistLocked writes the record now, rather than with helm's next one.
func (r *upgradeRecord) persistLocked() error {
	rel, err := r.driver.Get(r.key)
	if err != nil {
		return err
	}
	rel.Manifest = r.recorded
	markApplied(rel, r.recordedKnown)
	return r.driver.Update(r.key, rel)
}

// finish makes the release the upgrade returns carry what its revision stores.
func (r *upgradeRecord) finish(rel *release.Release) {
	if rel == nil || !r.attemptStarted() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rel.Manifest = r.recorded
	markApplied(rel, r.recordedKnown)
}

// recordKube is helm's KubeClient, except that the objects step goes through upgradeRecord.apply.
// An upgrade reaches that step through Update alone; hooks are created, never updated.
type recordKube struct {
	fullKube
	r *upgradeRecord
}

func (k *recordKube) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	if !k.r.attemptStarted() {
		return k.fullKube.Update(original, target, force)
	}
	return k.r.apply(original, target, force, k.fullKube.Update)
}

func (k *recordKube) UpdateThreeWayMerge(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	if !k.r.attemptStarted() {
		return k.fullKube.UpdateThreeWayMerge(original, target, force)
	}
	return k.r.apply(original, target, force, k.fullKube.UpdateThreeWayMerge)
}

// recordDriver stores this attempt's revision with the record. Every other revision passes through,
// including the rollback an Atomic upgrade writes after a failure.
type recordDriver struct {
	driver.Driver
	r *upgradeRecord
}

func (d *recordDriver) Create(key string, rel *release.Release) error {
	d.r.mu.Lock()
	defer d.r.mu.Unlock()
	if rel.Name != d.r.releaseName || d.r.key != "" {
		return d.Driver.Create(key, rel)
	}
	if rel.Version != d.r.last.Version+1 {
		// Another writer added a revision after the latest was read, so the baseline is not what is on
		// the cluster any more.
		return fmt.Errorf("revision %d is not the next one after %d: %w", rel.Version, d.r.last.Version, driver.ErrReleaseExists)
	}
	stored := *rel
	if d.r.known {
		stored.Manifest = d.r.baseline
	}
	markApplied(&stored, d.r.known)
	if err := d.Driver.Create(key, &stored); err != nil {
		return err
	}
	d.r.key, d.r.rendered, d.r.recorded, d.r.recordedKnown = key, rel.Manifest, stored.Manifest, d.r.known
	return nil
}

func (d *recordDriver) Update(key string, rel *release.Release) error {
	d.r.mu.Lock()
	defer d.r.mu.Unlock()
	if key != d.r.key {
		return d.Driver.Update(key, rel)
	}
	stored := *rel
	stored.Manifest = d.r.recorded
	markApplied(&stored, d.r.recordedKnown)
	return d.Driver.Update(key, &stored)
}
