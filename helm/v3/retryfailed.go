package helm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/resource"
	"sigs.k8s.io/yaml"
)

// The annotations helm stamps on every object it applies (action.setMetadataVisitor), which helm
// does not export.
const (
	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
)

// failedRetry is one attempt of InstallConfig.RetryFailed (core-provider#636): an action.Install with
// Replace whose OBJECTS step is computed against what the failed attempts left on the cluster.
//
// helm's own Install with Replace reads nothing from the revision it replaces: an object that is
// already live is merged with original == target, so a field the new render drops stays, and an object
// it drops stays for good, even through an uninstall. Everything else about the install is helm's, in
// helm's order: crds/, pre-install hooks, the objects, the wait, post-install hooks.
//
// Two seams do it, both belonging to this attempt's action.Configuration alone:
//
//   - the KubeClient (retryKube): the objects step, and only it, becomes one UpdateThreeWayMerge from
//     the baseline to the render;
//   - the storage driver (retryDriver): this attempt's revision stores what was APPLIED, not what was
//     rendered. Until the objects step has run that is the previous baseline, so an attempt that fails
//     in a pre-install hook does not hide what an earlier attempt applied.
type failedRetry struct {
	releaseName, namespace string
	kube                   fullKube
	driver                 driver.Driver
	force                  bool
	debugLog               action.DebugLog

	// baseline is the failed revision being retried, and baselineManifest its stored manifest without
	// the kinds the cluster no longer serves, which helm cannot build.
	baseline         *release.Release
	baselineManifest string
	// baselineKnown: the failed revision's manifest is a record of what was applied
	// (AppliedManifestLabel). A revision helm alone wrote stores its render, which may never have
	// reached the cluster.
	baselineKnown bool

	mu            sync.Mutex
	key           string // this attempt's storage key; empty until helm created its revision
	rendered      string // this attempt's render
	recorded      string // the manifest this attempt's revision stores
	recordedKnown bool   // recorded is what is applied, so the revision is marked (core-provider#640)
	applied       bool   // the objects step ran
}

// prepareRetryFailed checks that the release can be retried as an install and installs both seams on
// ac. It writes nothing.
func (c *client) prepareRetryFailed(releaseName, namespace string, cfg *helmconfig.InstallConfig, ac *action.Configuration) (*failedRetry, error) {
	history, err := ac.Releases.History(releaseName)
	if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, fmt.Errorf("retrying a failed install: reading the release history: %w", err)
	}
	if len(history) == 0 {
		return nil, fmt.Errorf("%w: %s has no revisions", helmconfig.ErrNotRetryableAsInstall, releaseName)
	}
	releaseutil.SortByRevision(history)
	for _, rel := range history {
		if rel.Info.Status == release.StatusDeployed {
			return nil, fmt.Errorf("%w: %s revision %d is deployed", helmconfig.ErrNotRetryableAsInstall, releaseName, rel.Version)
		}
	}
	last := history[len(history)-1]
	if last.Info.Status != release.StatusFailed {
		return nil, fmt.Errorf("%w: the latest revision of %s (%d) is %s", helmconfig.ErrNotRetryableAsInstall, releaseName, last.Version, last.Info.Status)
	}
	inner, ok := ac.KubeClient.(fullKube)
	if !ok {
		return nil, fmt.Errorf("retrying a failed install: the kube client %T cannot merge", ac.KubeClient)
	}

	mapper, err := ac.RESTClientGetter.ToRESTMapper()
	if err != nil {
		return nil, fmt.Errorf("retrying a failed install: rest mapper: %w", err)
	}
	manifest, dropped := filterUnmappableManifestDocs(last.Manifest, mapper)
	if len(dropped) > 0 && c.cachedClients != nil {
		// A kind is dropped from the baseline only when fresh discovery says it is not served: a stale
		// mapper would forget an object that is live.
		c.cachedClients.mapper.Reset()
		manifest, dropped = filterUnmappableManifestDocs(last.Manifest, mapper)
	}
	r := &failedRetry{
		releaseName: releaseName, namespace: namespace,
		kube: inner, driver: ac.Releases.Driver, force: cfg.Force, debugLog: c.debugLog,
		baseline: last, baselineManifest: manifest, baselineKnown: hasAppliedRecord(last),
	}
	if len(dropped) > 0 {
		r.logf("retrying %s as an install: revision %d names kinds the cluster no longer serves, left out of the merge: %s",
			releaseName, last.Version, strings.Join(dropped, ", "))
	}
	ac.KubeClient = &retryKube{fullKube: inner, r: r}
	ac.Releases.Driver = &retryDriver{Driver: ac.Releases.Driver, r: r}
	return r, nil
}

func (r *failedRetry) logf(format string, v ...interface{}) {
	if r.debugLog != nil {
		r.debugLog(format, v...)
		return
	}
	slog.Debug(fmt.Sprintf(format, v...))
}

// Run is a post-renderer that refuses a render which changes the pod template of a live Job, before
// helm writes the revision, runs a hook or applies an object (core-provider#403: the template is
// immutable, so the attempt could never be applied).
func (r *failedRetry) Run(rendered *bytes.Buffer) (*bytes.Buffer, error) {
	target, err := r.kube.Build(bytes.NewReader(rendered.Bytes()), false)
	if err != nil {
		return rendered, fmt.Errorf("failed to build rendered manifests: %w", err)
	}
	current, err := r.kube.Build(strings.NewReader(r.baselineManifest), false)
	if err != nil {
		return rendered, fmt.Errorf("failed to build the failed revision's manifest: %w", err)
	}
	var targetJobs kube.ResourceList
	for _, info := range target {
		if gvk := info.Object.GetObjectKind().GroupVersionKind(); gvk.Group == "batch" && gvk.Kind == "Job" {
			targetJobs = append(targetJobs, info)
		}
	}
	live, _ := readLive(targetJobs)
	if changed := classifyStoredJobs(current, targetJobs, live).templateChanged; len(changed) > 0 {
		return rendered, fmt.Errorf("%w: %s", helmconfig.ErrJobTemplateChanged, strings.Join(changed, ", "))
	}
	return rendered, nil
}

// attemptStarted reports whether helm has created this attempt's revision. Before it, helm's creates
// are the chart's crds/ and the release namespace.
func (r *failedRetry) attemptStarted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.key != ""
}

// apply is the objects step: one three-way merge from the baseline to the render.
func (r *failedRetry) apply(target kube.ResourceList) (*kube.Result, error) {
	built, err := r.kube.Build(strings.NewReader(r.baselineManifest), false)
	if err != nil {
		return nil, fmt.Errorf("building the failed revision's manifest: %w", err)
	}
	// What each baseline object was applied as. Taken before anything below reads or merges: helm
	// refreshes these objects in place.
	docs, err := documentsOf(built)
	if err != nil {
		return nil, err
	}

	// The objects to delete are those the render drops, and only if helm stamped them as this
	// release's. A failed attempt's manifest also names what it never created, and an object somebody
	// else created under such a name is not ours to delete.
	var current kube.ResourceList
	for _, info := range built {
		if target.Get(info) == nil {
			live, err := resource.NewHelper(info.Client, info.Mapping).Get(info.Namespace, info.Name)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("reading %s, which the render drops: %w", infoKey(info), err)
			}
			if !r.owns(live) {
				r.logf("retrying %s as an install: %s is dropped by the render but is not this release's, left alone", r.releaseName, infoKey(info))
				continue
			}
		}
		current = append(current, info)
	}

	// As Reconcile's 4.6, on BOTH sides: a Secret key written through stringData lives in data, so a
	// dropped one is only seen as dropped there.
	for _, info := range current {
		normalizeSecretStringData(info.Object)
	}
	for _, info := range target {
		normalizeSecretStringData(info.Object)
	}

	// As Reconcile's 4.7: a live target the baseline does not name is adopted as if the render had been
	// applied, so helm patches it instead of failing on a missing original.
	_, absent := readLive(target)
	for _, info := range target {
		if absent[infoKey(info)] || current.Get(info) != nil {
			continue
		}
		adopted := *info
		adopted.Object = info.Object.DeepCopyObject()
		current = append(current, &adopted)
	}

	// Before the first write: what is applied, plus what this attempt is about to create. A process
	// killed in the merge leaves this, so nothing it created is unrecorded.
	fresh, err := r.renderedDocuments()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.recorded, r.recordedKnown = recordIntent(current, target, docs, fresh), r.baselineKnown
	err = r.persistLocked()
	r.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("recording what is about to be applied: %w", err)
	}

	res, err := r.kube.UpdateThreeWayMerge(current, target, r.force)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = true
	if err == nil {
		r.recorded, r.recordedKnown = recordSuccess(r.rendered, current, target, res, docs), true
	} else {
		r.recorded, r.recordedKnown = recordPartial(current, target, docs, fresh), r.baselineKnown
	}
	// Best effort: helm's own record, after the wait and the post-install hooks, carries the same
	// manifest, and the one written above already names every object.
	if perr := r.persistLocked(); perr != nil {
		r.logf("retrying %s as an install: recording what was applied: %v", r.releaseName, perr)
	}
	return res, err
}

// recordIntent is the manifest just before the merge: every object as it was applied, and the render
// of each one not applied yet.
func recordIntent(current, target kube.ResourceList, docs, fresh map[string]string) string {
	var out []string
	for _, info := range current {
		if doc, ok := docs[infoKey(info)]; ok {
			out = append(out, doc)
		}
	}
	for _, info := range target {
		if before := current.Get(info); before != nil {
			if _, ok := docs[infoKey(before)]; ok {
				continue
			}
		}
		out = append(out, fresh[infoKey(info)])
	}
	return strings.Join(out, "\n---\n")
}

// renderedDocuments is the render's documents, built apart from the list helm applies: that one
// carries helm's ownership metadata and is refreshed in place.
func (r *failedRetry) renderedDocuments() (map[string]string, error) {
	fresh, err := r.kube.Build(strings.NewReader(r.rendered), false)
	if err != nil {
		return nil, fmt.Errorf("building the rendered manifest: %w", err)
	}
	return documentsOf(fresh)
}

// recordSuccess is the manifest after a merge helm reports as done: the render, plus each dropped
// object whose deletion helm asked for and did not get (it logs those and carries on).
func recordSuccess(rendered string, current, target kube.ResourceList, res *kube.Result, docs map[string]string) string {
	var deleted kube.ResourceList
	if res != nil {
		deleted = res.Deleted
	}
	out := rendered
	_, notDone, unknown := pendingDeletions(current, target, deleted, getLive)
	for _, key := range append(notDone, unknown...) {
		if doc, ok := docs[key]; ok {
			out += "\n---\n" + doc
		}
	}
	return out
}

// recordPartial is the manifest after a merge that failed, object by object: the render where helm
// confirmed the write, or where there is nothing else it could be; what was applied before where the
// write failed; and every dropped object, because helm deletes only after every update succeeded.
func recordPartial(current, target kube.ResourceList, docs, fresh map[string]string) string {
	var out []string
	for _, info := range target {
		if before := current.Get(info); before != nil && !writeConfirmed(info) {
			if doc, ok := docs[infoKey(before)]; ok {
				out = append(out, doc)
				continue
			}
		}
		out = append(out, fresh[infoKey(info)])
	}
	for _, info := range current.Difference(target) {
		if doc, ok := docs[infoKey(info)]; ok {
			out = append(out, doc)
		}
	}
	return strings.Join(out, "\n---\n")
}

// persistLocked writes the record now, rather than with helm's next one.
func (r *failedRetry) persistLocked() error {
	rel, err := r.driver.Get(r.key)
	if err != nil {
		return err
	}
	rel.Manifest = r.recorded
	markApplied(rel, r.recordedKnown)
	return r.driver.Update(r.key, rel)
}

// finish completes an install that succeeded. A render with no objects makes helm skip the objects
// step entirely, so what the failed attempts left is removed here.
func (r *failedRetry) finish(rel *release.Release) error {
	if !r.attemptStarted() {
		return nil // a dry run
	}
	r.mu.Lock()
	applied := r.applied
	r.mu.Unlock()
	var err error
	if !applied {
		_, err = r.apply(nil)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rel.Manifest = r.recorded
	markApplied(rel, r.recordedKnown)
	if err != nil {
		return fmt.Errorf("removing what the failed install left: %w", err)
	}
	return nil
}

// owns reports whether helm stamped obj as this release's.
func (r *failedRetry) owns(obj runtime.Object) bool {
	return annotationOf(obj, helmReleaseNameAnnotation) == r.releaseName &&
		annotationOf(obj, helmReleaseNamespaceAnnotation) == r.namespace
}

// documentsOf is each object's YAML document, keyed by infoKey.
func documentsOf(list kube.ResourceList) (map[string]string, error) {
	out := make(map[string]string, len(list))
	for _, info := range list {
		doc, err := yaml.Marshal(info.Object)
		if err != nil {
			return nil, fmt.Errorf("serializing %s: %w", infoKey(info), err)
		}
		out[infoKey(info)] = string(doc)
	}
	return out, nil
}

// retryKube is the KubeClient of a failedRetry: helm's, except that the objects step is the retry's
// merge. helm reaches that step through Create when no target is live and through Update or
// UpdateThreeWayMerge otherwise, and through nothing else once the attempt's revision exists, except
// for hooks, which it creates in their own calls.
type retryKube struct {
	fullKube
	r *failedRetry
}

func (k *retryKube) Create(resources kube.ResourceList) (*kube.Result, error) {
	if !k.r.attemptStarted() || allHooks(resources) {
		return k.fullKube.Create(resources)
	}
	return k.r.apply(resources)
}

func (k *retryKube) Update(_, target kube.ResourceList, _ bool) (*kube.Result, error) {
	return k.r.apply(target)
}

func (k *retryKube) UpdateThreeWayMerge(_, target kube.ResourceList, _ bool) (*kube.Result, error) {
	return k.r.apply(target)
}

// retryDriver stores this attempt's revision with the manifest that was applied. Every other record
// passes through.
type retryDriver struct {
	driver.Driver
	r *failedRetry
}

func (d *retryDriver) Create(key string, rel *release.Release) error {
	d.r.mu.Lock()
	defer d.r.mu.Unlock()
	if rel.Name != d.r.releaseName {
		return d.Driver.Create(key, rel)
	}
	if rel.Version != d.r.baseline.Version+1 {
		// Another writer added a revision after the baseline was read, so the baseline is not what is
		// on the cluster any more.
		return fmt.Errorf("revision %d is not the next one after %d: %w", rel.Version, d.r.baseline.Version, driver.ErrReleaseExists)
	}
	stored := *rel
	stored.Manifest = d.r.baseline.Manifest
	markApplied(&stored, d.r.baselineKnown)
	if err := d.Driver.Create(key, &stored); err != nil {
		return err
	}
	d.r.key, d.r.rendered, d.r.recorded, d.r.recordedKnown = key, rel.Manifest, d.r.baseline.Manifest, d.r.baselineKnown
	return nil
}

func (d *retryDriver) Update(key string, rel *release.Release) error {
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

// retryInstall runs installClient as a retry of the release's failed revision.
func (c *client) retryInstall(ctx context.Context, installClient *action.Install, run func(context.Context) (*release.Release, error), namespace string, cfg *helmconfig.InstallConfig, ac *action.Configuration) (*release.Release, error) {
	r, err := c.prepareRetryFailed(installClient.ReleaseName, namespace, cfg, ac)
	if err != nil {
		return nil, err
	}
	installClient.Replace = true
	installClient.PostRenderer = postRendererChain{renderers: []helmconfig.PostRenderer{installClient.PostRenderer, r}}
	rel, err := run(ctx)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseExists) {
			return nil, fmt.Errorf("%w: %w", helmconfig.ErrOperationInProgress, err)
		}
		return nil, err
	}
	if err := r.finish(rel); err != nil {
		return nil, err
	}
	return rel, nil
}
