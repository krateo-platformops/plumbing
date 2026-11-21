package helm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/kube"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest"
)

// UpgradeConfig.WriteNothing (core-provider#597). Reconcile under it runs on a copy of the client
// whose rest config sends every request that is not a read as a server-side dry run. That is the
// whole guarantee: nothing below has to remember not to write, because no client it can reach is
// able to. The merge it runs is helm's own, with helm's own patches; the apiserver answers each with
// the object it WOULD store, and step 6 compares that against the live object exactly as it does a
// real write's result.

// writeNothingConflicts refuses the options that only mean something when Reconcile writes.
func writeNothingConflicts(cfg *helmconfig.UpgradeConfig) error {
	var bad []string
	if cfg.Force {
		// helm's force path is delete-then-recreate: the dry-run delete leaves the object, so the
		// dry-run re-create would read AlreadyExists and look like a merge failure.
		bad = append(bad, "Force")
	}
	if cfg.Wait {
		bad = append(bad, "Wait")
	}
	if cfg.WaitForJobs {
		bad = append(bad, "WaitForJobs")
	}
	if cfg.Atomic {
		bad = append(bad, "Atomic")
	}
	if len(bad) > 0 {
		return fmt.Errorf("reconcile: %w: %s", helmconfig.ErrWriteNothingConflict, strings.Join(bad, ", "))
	}
	return nil
}

// dryRunRefusal is one dry-run request an admission webhook denied.
type dryRunRefusal struct {
	method, path, message string
}

// dryRunLog collects the refusals the transport saw, for Reconcile to classify after the merge.
// helm's update flattens per-object patch errors into one string (pkg/kube client.go, joined with
// " && "), so the typed status is gone by the time Reconcile sees the error; the transport sees the
// server's own response, which is where the classification belongs.
type dryRunLog struct {
	mu       sync.Mutex
	refusals []dryRunRefusal
}

func (l *dryRunLog) add(r dryRunRefusal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refusals = append(l.refusals, r)
}

func (l *dryRunLog) all() []dryRunRefusal {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]dryRunRefusal(nil), l.refusals...)
}

// err is ErrDryRunRefused naming every refused request, or nil.
func (l *dryRunLog) err() error {
	rs := l.all()
	if len(rs) == 0 {
		return nil
	}
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%s %s: %s", r.method, r.path, r.message))
	}
	return fmt.Errorf("reconcile: %w: %s", helmconfig.ErrDryRunRefused, strings.Join(parts, "; "))
}

// dryRunTransport adds dryRun=All to every request that is not a read. GET (which includes LIST and
// WATCH), HEAD and OPTIONS pass unchanged.
type dryRunTransport struct {
	next http.RoundTripper
	log  *dryRunLog
}

func isReadMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (t *dryRunTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if isReadMethod(r.Method) {
		return t.next.RoundTrip(r)
	}
	r = r.Clone(r.Context()) // a RoundTripper must not modify the request it was given
	q := r.URL.Query()
	q.Set("dryRun", metav1.DryRunAll)
	r.URL.RawQuery = q.Encode()
	if r.Method == http.MethodDelete {
		if err := dryRunDeleteBody(r); err != nil {
			return nil, err // fail closed: a delete whose options cannot be made a dry run is not sent
		}
	}
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp == nil || resp.StatusCode != http.StatusForbidden || resp.Body == nil {
		return resp, err
	}
	body, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if rerr != nil {
		return resp, nil
	}
	if msg, ok := admissionDenial(body); ok {
		t.log.add(dryRunRefusal{method: r.Method, path: r.URL.Path, message: msg})
	}
	return resp, nil
}

// dryRunDeleteBody makes a DELETE's body options a dry run too. The query parameter alone is NOT
// enough: helm sends DeleteOptions (its propagation policy) in the request body, and when a body is
// present the apiserver decodes the options from it and ignores the query (measured on
// kube-apiserver 1.36: a DELETE with ?dryRun=All and a DeleteOptions body without dryRun deleted the
// object). A body that is not a JSON object is refused rather than sent.
func dryRunDeleteBody(r *http.Request) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return fmt.Errorf("write-nothing: reading a DELETE body: %w", err)
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		var opts map[string]any
		if err := json.Unmarshal(raw, &opts); err != nil {
			return fmt.Errorf("write-nothing: refusing a DELETE whose body is not JSON options, so it cannot be made a dry run: %w", err)
		}
		opts["dryRun"] = []string{metav1.DryRunAll}
		if raw, err = json.Marshal(opts); err != nil {
			return fmt.Errorf("write-nothing: encoding DELETE options: %w", err)
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	return nil
}

// admissionDenial reports whether a 403 body is an admission webhook's denial, as the apiserver
// writes it: a Status whose message starts `admission webhook "<name>" denied the request` (its
// reason is empty, measured on kube-apiserver 1.36, so the message is the only discriminator the
// server gives).
func admissionDenial(body []byte) (string, bool) {
	var st metav1.Status
	if err := json.Unmarshal(body, &st); err != nil || st.Kind != "Status" {
		return "", false
	}
	if strings.HasPrefix(st.Message, "admission webhook ") && strings.Contains(st.Message, " denied the request") {
		return st.Message, true
	}
	return "", false
}

// writeNothingClient is c with every client it builds sending non-read requests as dry runs.
func (c *client) writeNothingClient(log *dryRunLog) *client {
	dc := *c
	cfg := rest.CopyConfig(c.restConfig)
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return &dryRunTransport{next: rt, log: log} })
	dc.restConfig = cfg
	dc.dryRun = log
	return &dc
}

// dryRunCreateConfirmed reports whether the apiserver accepted a dry-run create of info. helm sets
// info.Object to the server's response only when the create succeeds, and leaves the render (no
// metadata.uid) otherwise. writeConfirmed cannot be used: a dry-run create's response carries NO
// resourceVersion (measured on kube-apiserver 1.36: rv="" with uid and creationTimestamp set), so
// every dry-run create would read as unconfirmed and a child deleted out of band would read as no
// change. A dry-run patch's response keeps the live resourceVersion (measured: unchanged), so
// writeConfirmed still holds for patches.
func dryRunCreateConfirmed(info *resource.Info) bool {
	a, err := apimeta.Accessor(info.Object)
	return err == nil && a.GetUID() != ""
}

// writeNothingResult is step 6 for UpgradeConfig.WriteNothing: the same before/after comparison as a
// real merge, over the apiserver's dry-run answers, reported in the Would* fields only. Nothing that
// a caller records as a write (Changed, Created, Deleted, PatchedUpdated, ChangedObjects, Objects,
// IntendedDeletions, KeptDeletions) is set.
func (c *client) writeNothingResult(stored *helmconfig.Release, res *kube.Result, current, target kube.ResourceList,
	liveObj map[string]runtime.Object, absent map[string]bool, owned map[string]fieldMask, stale *changedObjectSet,
	jobs storedJobs, applyErr error) (*helmconfig.ReconcileResult, error) {
	creates := newChangedObjectSet(maxChangedObjects)
	for _, info := range res.Created {
		if dryRunCreateConfirmed(info) {
			creates.add(infoKey(info))
		}
	}
	deletes := newChangedObjectSet(maxChangedObjects)
	for _, info := range res.Deleted { // helm appends a child only after its (dry-run) delete succeeded
		deletes.add(infoKey(info))
	}
	patches := newChangedObjectSet(maxChangedObjects)
	for _, info := range target {
		key := infoKey(info)
		if absent[key] || !writeConfirmed(info) {
			continue
		}
		changed, err := semanticallyChanged(liveObj[key], info.Object, owned[key])
		if err != nil || changed { // undetermined counts as a change, as in step 6
			patches.add(key)
		}
	}
	// Kept deletions change the release record (a new revision), not any child.
	kept, _, _ := pendingDeletions(current, target, res.Deleted, getLive)

	r := &helmconfig.ReconcileResult{
		WriteWithheld:                true,
		Release:                      stored,
		ObjectsComplete:              true,
		WouldCreate:                  creates.total(),
		WouldCreateObjects:           creates.names,
		WouldCreateObjectsOmitted:    creates.omitted,
		WouldDelete:                  deletes.total(),
		WouldDeleteObjects:           deletes.names,
		WouldDeleteObjectsOmitted:    deletes.omitted,
		WouldPatch:                   patches.total(),
		WouldPatchObjects:            patches.names,
		WouldPatchObjectsOmitted:     patches.omitted,
		StaleManifestVersions:        stale.names,
		StaleManifestVersionsOmitted: stale.omitted,
		ChangedJobTemplates:          jobs.templateChanged,
	}
	r.WouldChange = r.WouldCreate+r.WouldDelete+r.WouldPatch > 0 || len(kept) > 0 || stale.total() > 0 ||
		stored.Status == helmconfig.StatusFailed

	var errs []error
	if rerr := c.dryRun.err(); rerr != nil {
		errs = append(errs, rerr)
	}
	if len(jobs.templateChanged) > 0 {
		errs = append(errs, fmt.Errorf("reconcile: %w: %s", helmconfig.ErrJobTemplateChanged, strings.Join(jobs.templateChanged, ", ")))
	}
	if applyErr != nil {
		errs = append(errs, applyErr)
	}
	return r, errors.Join(errs...)
}
