package helm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"k8s.io/client-go/rest"
)

// Release is a version-agnostic representation of a Helm release.
// We do not import helm.sh/helm/... here.
type Release struct {
	Name         string
	Namespace    string
	Revision     int
	ChartVersion string
	Status       Status
	Values       map[string]interface{}
	Manifest     string
	// Updated is helm's Info.LastDeployed for this revision — when the (possibly still
	// pending) revision record was written. For a pending-install/upgrade/rollback status,
	// time.Since(Updated) is how long the operation has been in flight, which callers use to
	// tell a legitimately in-progress operation from a genuinely stuck one.
	Updated time.Time

	// Hooks are the release's lifecycle hooks. helm keeps them OUT of Manifest by construction —
	// releaseutil.SortManifests splits them into their own slice — so a consumer that reads only
	// Manifest cannot see them at all.
	//
	// WHY THIS IS EXPOSED (core-provider#311). chart-inspector derives a composition's RBAC from a
	// dry-run install: the tracer records the API calls, and the result is narrowed against
	// Manifest. A dry run executes no hooks and Manifest excludes them, so NO grant was ever
	// generated for a hook object. A `pre-delete` hook therefore turned teardown into an uninstall
	// that could never succeed — measured on a real cluster, with the controlled comparison being
	// one template rendering four objects of which only the hook-annotated one went ungranted.
	//
	// The hooks are available on a DRY RUN: helm populates rel.Hooks at action/install.go:317 and
	// returns early for a dry run at :364 — after. And collection is PHASE-AGNOSTIC: SortManifests
	// gathers every hook and merely records its Events, so a pre-delete hook is present in the
	// result of an install probe.
	Hooks []Hook
}

// Hook is one lifecycle hook of a release, projected from helm's release.Hook.
//
// PROJECTED RATHER THAN RE-EXPORTED, deliberately. Release exists so a consumer does not take a
// direct dependency on helm's types — chart-inspector has helm.sh/helm/v3 as an INDIRECT
// requirement and should keep it that way. Re-exporting []*release.Hook, or helm's HookEvent, would
// make helm direct for every consumer that touches a hook. Only the fields a consumer needs are
// carried; everything else about a hook (weight, delete policies, last run) stays helm's business.
type Hook struct {
	// Name is the hook object's name, and Kind its Kubernetes kind.
	Name string
	Kind string
	// Path is the chart-relative template path, which is what makes a hook diagnosable: an
	// operator told "this hook has no RBAC" needs to know which file to look at.
	Path string
	// Manifest is the rendered document. It carries the apiVersion and namespace that Kind alone
	// does not, so a consumer resolving a GVK must parse this rather than infer from Kind.
	Manifest string
	// Events are the hook's phases ("pre-delete", "post-install", ...) as plain strings.
	//
	// A consumer deciding RBAC must NOT filter on these: an object needs its permissions
	// regardless of which phase fires it, and helm deletes a previous hook object under
	// before-hook-creation whatever its phase. Carried because a diagnostic should be able to say
	// WHICH hook, not so the set can be narrowed.
	Events []string
}

// GetHooks returns the release's lifecycle hooks. Mirrors GetManifest.
func (r *Release) GetHooks() []Hook {
	return r.Hooks
}

func (r *Release) String() string {
	return fmt.Sprintf("Release{Name: %s, Namespace: %s, Revision: %d, ChartVersion: %s, Status: %s}", r.Name, r.Namespace, r.Revision, r.ChartVersion, r.Status)
}

func (r *Release) GetManifest() string {
	return r.Manifest
}

// ReconcileResult reports what a Reconcile did to converge the cluster to the
// rendered chart. Changed is true iff the live cluster was actually mutated
// (a child was created, deleted, or patched with a non-empty diff). When
// Changed is false the cluster already matched the desired manifest and NO new
// helm revision was written and NO hooks ran.
type ReconcileResult struct {
	Changed        bool
	Created        int
	Deleted        int
	PatchedUpdated int
	Release        *Release
	// ChangedObjects identifies the children this reconcile created, deleted or patched, as
	// "group/version/Kind/namespace/name". The counts above say THAT something drifted;
	// core-provider#175's open half is that nothing said WHICH child — so an operator seeing
	// "patched=1" a hundred times an hour had no way to reach the cause. Diagnosing one such case
	// took a cluster-wide differential across five compositions; this line would have named the
	// child outright.
	//
	// Free to collect: the loop that decides `patched` already computes this identity per child
	// (infoKey) and already holds both the live and target objects. No extra read, no extra decode,
	// no new comparison.
	//
	// CAPPED, and the cap is part of the design rather than tidiness. The composition that prompted
	// this writes two revisions a minute, so an uncapped list would turn a diagnostic into the same
	// log-volume problem #175 is about.
	ChangedObjects []string
	// ChangedObjectsOmitted is how many identities exceeded the cap and were not listed, so a
	// reader can tell a genuinely short list from a truncated one.
	ChangedObjectsOmitted int
	// ChangedPaths names, for each child in ChangedObjects that the merge PATCHED (keyed by the same
	// identity), the JSON paths whose persisted content differed, e.g. "metadata.annotations[checksum/config]"
	// or "spec.replicas" (plumbing#112). changedObjects says WHICH child drifted; this says which field,
	// which is what a drift investigation needs and what a managedFields snapshot taken after the heal
	// cannot give (the heal takes ownership). PATHS ONLY, never values: a value may be Secret data or a
	// token. Capped at a few paths per child (ChangedPathsOmitted counts the rest). Created and deleted
	// children have no entry: the whole object is the change.
	ChangedPaths map[string][]string
	// ChangedPathsOmitted is, per child, how many changed paths exceeded the cap.
	ChangedPathsOmitted map[string]int
	// AppliedPartially reports that the self-heal apply converged SOME children and failed on
	// others (core-provider#276). When it is true, Reconcile returns BOTH this result and a
	// non-nil error, and the error names the children that failed.
	//
	// WHY THE RESULT IS RETURNED AT ALL ON ERROR. helm's own update collects per-object errors and
	// CONTINUES — pkg/kube/client.go appends to updateErrors, appends to res.Updated "regardless",
	// and returns a populated Result alongside the joined error. So the other children really did
	// converge. Reconcile used to answer that with `return nil, err`, throwing the populated result
	// away: the change detection below never ran, the stored digest never advanced, and a
	// composition reported total failure while most of it was healthy. Observed on a0, where six
	// orphaned Jobs from an earlier release wedged a Keystone composition permanently.
	//
	// A caller that follows the Go idiom and checks err before touching the result will see only a
	// failure, which is the OLD behaviour and remains safe. A caller that wants to distinguish
	// "nothing converged" from "most of it converged" must check this field — hence a named bool
	// rather than relying on result != nil, which is easy to read past.
	//
	// No revision is persisted on a partial apply: the real Upgrade that would write one patches the
	// same objects and would fail the same way, so Reconcile returns before it.
	AppliedPartially bool
	// IntendedDeletions identifies the children the merge was ASKED to delete — those present in
	// the stored manifest and absent from the render — as "group/version/Kind/namespace/name".
	//
	// WHY INTENT AND NOT JUST THE OUTCOME (core-provider#304). `Deleted` reports what helm says it
	// removed. On its own it cannot distinguish three states that matter:
	//
	//	intended 0, deleted 0   nothing was removed from the render. Correct, and the common case.
	//	intended 1, deleted 1   the removal converged.
	//	intended 1, deleted 0   THE DEFECT. Either helm's deletion loop never ran — it sits AFTER
	//	                        the error switch in pkg/kube/client.go, so one unpatchable object
	//	                        skips every deletion — or the delete was issued and did not stick.
	//
	// A caller seeing only `deleted: 0` cannot tell the first from the third, which is exactly how
	// #304 went unnoticed: a node removed from a spec left its child alive for sixteen minutes
	// while the log said deleted:0, and that same line is printed by every healthy steady-state
	// reconcile.
	//
	// Computed with helm's OWN ResourceList.Difference against the same two lists the merge
	// receives, so it matches helm's matching exactly (group+kind+name+namespace, version-
	// independent via isMatchingInfo) rather than replicating it. Taken after filterLiveJobs and
	// the adoption pass, so it reflects the lists as actually handed over.
	IntendedDeletions []string
	// IntendedDeletionsOmitted is how many identities exceeded the cap and were not listed, so a
	// reader can tell a genuinely short list from a truncated one.
	IntendedDeletionsOmitted int

	// KeptDeletions names the intended deletions helm did NOT perform because the live object carries
	// helm.sh/resource-policy: keep (core-provider, 2026-11-08). helm is right to leave them alive;
	// what must still happen is that the release RECORDS the change. Each one therefore counts toward
	// Changed, so the real Upgrade writes a revision with the new values and manifest, and the release
	// stops claiming the object. A deletion that was intended, not kept and not performed (its delete
	// failed) is deliberately NOT here: counting it would write a revision without the object, helm
	// would forget it, and the delete would never be retried.
	KeptDeletions []string
	// KeptDeletionsOmitted is how many kept identities exceeded the cap and were not listed.
	KeptDeletionsOmitted int
	// ChangedJobTemplates names the live Jobs (namespace/name) whose pod template the CHART changed
	// between the stored release and this render (core-provider#403). A Job's spec.template is
	// immutable, so such a release cannot be applied; Reconcile returns ErrJobTemplateChanged
	// alongside this list. Empty when no such Job exists.
	ChangedJobTemplates []string
	// RetriedFailedRelease is true when the stored latest revision was "failed" and this reconcile
	// therefore ran the real upgrade again rather than treating that revision's manifest as
	// converged (core-provider#403).
	RetriedFailedRelease bool

	// Objects is the per-child verdict of the self-heal merge (plumbing#106), one entry per child
	// it created, deleted, changed, kept or failed to delete, sorted by identity. It is UNCAPPED,
	// unlike ChangedObjects and the other diagnostic lists, because a caller writes one audit record
	// per entry (AUDIT-4) and a dropped entry would be a missing record. Keep the capped lists for
	// log lines; never log this one whole.
	//
	// A child the merge left as it was is absent, including one where helm sent a patch that the
	// apiserver accepted as a no-op: the cluster keeps no trace of it, so there is no change to
	// record. Jobs the self-heal keeps out of the merge (#276, #403) are absent too, since nothing
	// was sent for them.
	//
	// Names and the resource only: no manifest, no field values, no Secret data.
	//
	// Empty when ObjectsComplete is false.
	Objects []ObjectVerdict
	// ObjectsComplete is false when Reconcile could not say which children changed: the release
	// did not exist yet and Reconcile installed it through Upgrade, which reports no per-object
	// outcome. On every other path (a clean or partial apply, an ErrJobTemplateChanged return)
	// Objects is complete for what the merge did.
	ObjectsComplete bool
}

// ObjectVerdict is one child's outcome in a Reconcile (plumbing#106).
type ObjectVerdict struct {
	Group     string
	Version   string
	Resource  string // the plural resource name, e.g. "deployments"
	Kind      string
	Namespace string // empty for a cluster-scoped child
	Name      string
	// UID is the child's uid when the merge saw it: the created or patched object, or the live
	// object of a kept deletion. Empty when unknown.
	UID string
	// Verb is what was sent to the apiserver: "create", "patch" (or "update" under Force, which
	// replaces), or "delete". A kept deletion and a deletion that was not done carry "delete", the
	// verb the release asked for, although helm sent nothing for a kept one.
	Verb   string
	Change ObjectChange
}

// ObjectChange is how a child changed. See SpecChanged for how each counts.
type ObjectChange string

const (
	// ChangeCreated: the child did not exist and the merge created it.
	ChangeCreated ObjectChange = "created"
	// ChangeDeleted: the child left the render and the merge deleted it (helm pruned it).
	ChangeDeleted ObjectChange = "deleted"
	// ChangeSpec: the merge changed the child's content, its fields other than apiVersion, kind,
	// metadata and status. That is .spec where there is one, and data/type for a Secret or a
	// ConfigMap. RBAC rules and subjects compare as sets and a Secret by its effective data, so
	// reordering a list or writing the same value through stringData is not a change.
	ChangeSpec ObjectChange = "specChanged"
	// ChangeMetadataOnly: the merge changed the child, but only its metadata (labels,
	// annotations, finalizers, owner references).
	ChangeMetadataOnly ObjectChange = "metadataOnly"
	// ChangeCompareFailed: the merge was applied to the child but the comparison of its before and
	// after state failed, so nothing can be said about what changed. Reconcile counts it as a
	// change (fail safe); a caller must not read it as a content change.
	ChangeCompareFailed ObjectChange = "compareFailed"
	// ChangeKept: the child left the render, and helm declined to delete it because the live
	// object carries helm.sh/resource-policy: keep.
	ChangeKept ObjectChange = "kept"
	// ChangeDeleteNotDone: the child left the render, is still live, and is not kept: its deletion
	// failed, or helm never reached its deletion loop. The next reconcile asks again.
	ChangeDeleteNotDone ObjectChange = "deleteNotDone"
)

// SpecChanged reports whether the change counts as a content change: a create or a delete
// (absent to present, present to absent) or a content change. A metadata-only change, a failed
// comparison, a kept deletion and a deletion not done do not.
func (c ObjectChange) SpecChanged() bool {
	return c == ChangeCreated || c == ChangeDeleted || c == ChangeSpec
}

// ErrJobTemplateChanged is returned (wrapped) by Reconcile when the chart changed the pod template of
// a Job that is live. The template is immutable, so the release cannot converge until the chart stops
// changing it (a hook with before-hook-creation, or a versioned Job name) or the Job is removed.
var ErrJobTemplateChanged = errors.New("the chart changed the pod template of a live Job, which is immutable")

// Client is the interface that the rest of your app uses.
// It returns your custom *Release, not the Helm SDK struct.
type Client interface {
	Install(ctx context.Context, releaseName string, chartRef string, config *InstallConfig) (*Release, error)
	Upgrade(ctx context.Context, releaseName string, chartRef string, config *UpgradeConfig) (*Release, error)
	// Reconcile performs a fork-free, self-healing "apply-if-changed" reconcile:
	// it converges the live cluster to the rendered chart (recreating deleted
	// children and patching drifted fields via helm's 3-way merge) and writes a
	// new revision + runs hooks ONLY when the cluster was actually mutated. At
	// steady state it is a no-op (no revision, no hooks). See the v3 impl for the
	// exact change-detection semantics.
	Reconcile(ctx context.Context, releaseName string, chartRef string, config *UpgradeConfig) (*ReconcileResult, error)
	Uninstall(ctx context.Context, releaseName string, config *UninstallConfig) error
	Rollback(ctx context.Context, releaseName string, config *RollbackConfig) (*Release, error)
	GetRelease(ctx context.Context, releaseName string, config *GetConfig) (*Release, error)
	ListReleases(ctx context.Context, config *ListConfig) ([]*Release, error)
	Close() error
}

type InstallConfig struct {
	*ActionConfig
	Namespace  string
	RestConfig *rest.Config
	// Install-Only Fields
	GenerateName    bool
	CreateNamespace bool
}

type UpgradeConfig struct {
	*ActionConfig
	// Upgrade-Only Fields
	Install    bool
	MaxHistory int
	// NO ReuseValues / ResetValues / ResetThenReuseValues, deliberately (#115). They were declared
	// here and never copied onto helm's action.Upgrade (applyUpgradeConfig), so setting one did
	// nothing, silently. The upgrade contract is "fresh values rendered against the NEW chart's
	// defaults", and honouring ReuseValues would reintroduce the --reuse-values render crash class
	// (an old release's values without a key a new template dereferences: keycloak-config-kog 0.5.0,
	// "nil pointer evaluating interface {}.enabled").
}

type RollbackConfig struct {
	ReleaseVersion int
	Timeout        time.Duration
	Wait           bool
	WaitForJobs    bool
	DisableHooks   bool
	DryRun         bool
	Recreate       bool // will (if true) recreate pods after a rollback.
	Force          bool // will (if true) force resource upgrade through uninstall/recreate if needed
	CleanupOnFail  bool
	MaxHistory     int // MaxHistory limits the maximum number of revisions saved per release
}

type UninstallConfig struct {
	DisableHooks        bool
	DryRun              bool
	IgnoreNotFound      bool
	KeepHistory         bool
	Wait                bool
	DeletionPropagation string
	Timeout             time.Duration
	Description         string
}

// ActionConfig holds all possible configuration values for Install AND Upgrade.
// It acts as a data transfer object between your app and the specific Helm implementation.
type ActionConfig struct {
	ChartVersion          string
	ChartName             string
	Values                map[string]interface{}
	Description           string
	Timeout               time.Duration
	Wait                  bool
	WaitForJobs           bool
	Atomic                bool
	PostRenderer          PostRenderer // Custom interface defined below
	Labels                map[string]string
	Replace               bool
	Force                 bool
	DryRun                DryRunMode
	TakeOwnership         bool
	SkipSchemaValidation  bool
	SkipCRDs              bool
	Verify                bool
	NoHooks               bool
	DependencyUpdate      bool
	Devel                 bool
	IncludeCRDs           bool
	SubNotes              bool
	HideNotes             bool
	EnableDNS             bool
	InsecureSkipTLSverify bool

	Username           string
	Password           string
	PassCredentialsAll bool
	Verbose            bool
	LogWriter          io.Writer
}

type GetConfig struct {
	Version int
	// Namespace overrides the client's namespace for this lookup. Empty means the client's own.
	//
	// A release is identified by name AND namespace, so without this a caller could only ask
	// about releases in the namespace the client was built for — and callers that install into
	// several namespaces (operators, test harnesses standing up prerequisites) had no way to ask
	// "does this release already exist?" at all. Mirrors InstallConfig.Namespace and
	// UpgradeConfig's handling, so the same override means the same thing on every verb.
	Namespace string
}

type ListConfig struct {
	// All ignores the limit/offset
	All bool
	// AllNamespaces searches across namespaces
	AllNamespaces bool
	// Sort indicates the sort to use
	Sort Sorter
	// Overrides the default lexicographic sorting
	ByDate      bool
	SortReverse bool
	// StateMask accepts a bitmask of states for items to show.
	//
	// LEAVE IT AT ZERO TO GET HELM'S OWN DEFAULT, which is ListDeployed|ListFailed — the same set
	// the `helm list` CLI shows. A zero value means "not specified" and is NOT applied
	// (plumbing#74); before that fix it was assigned through, and helm skips every release when
	// the mask is 0, so an unset field returned an empty slice with a nil error.
	//
	// The previous text here said "The default is ListDeployed", copied from helm's own comment at
	// pkg/action/list.go:119 — which is wrong about its own code at :142. The CLI's `list --help`
	// is the accurate statement: "By default, it lists only releases that are deployed or failed."
	// So a FAILED release is not hidden by default; what --all adds is superseded, uninstalled,
	// uninstalling and the pending-* family, and `pending-install` is the one that hides an install
	// that never completed.
	StateMask ListStates
	// Limit is the number of items to return per Run()
	Limit int
	// Offset is the starting index for the Run() call
	Offset int
	// Filter is a filter that is applied to the results
	Filter       string
	Short        bool
	NoHeaders    bool
	TimeFormat   string
	Uninstalled  bool
	Superseded   bool
	Uninstalling bool
	Deployed     bool
	Failed       bool
	Pending      bool
	Selector     string
}

// --- Custom Interfaces ---
// We duplicate the PostRenderer interface here so users can implement it
// without importing the Helm SDK directly.
type PostRenderer interface {
	// Run expects the rendered manifests and returns modified manifests
	Run(renderedManifests *bytes.Buffer) (*bytes.Buffer, error)
}

type DryRunMode int

const (
	DryRunNone DryRunMode = iota
	DryRunClient
	DryRunServer
)

type ListStates uint

const (
	// ListDeployed filters on status "deployed"
	ListDeployed ListStates = 1 << iota
	// ListUninstalled filters on status "uninstalled"
	ListUninstalled
	// ListUninstalling filters on status "uninstalling" (uninstall in progress)
	ListUninstalling
	// ListPendingInstall filters on status "pending" (deployment in progress)
	ListPendingInstall
	// ListPendingUpgrade filters on status "pending_upgrade" (upgrade in progress)
	ListPendingUpgrade
	// ListPendingRollback filters on status "pending_rollback" (rollback in progress)
	ListPendingRollback
	// ListSuperseded filters on status "superseded" (historical release version that is no longer deployed)
	ListSuperseded
	// ListFailed filters on status "failed" (release version not deployed because of error)
	ListFailed
	// ListUnknown filters on an unknown status
	ListUnknown
)

type Sorter uint

const (
	// ByNameDesc sorts by descending lexicographic order
	ByNameDesc Sorter = iota + 1
	// ByDateAsc sorts by ascending dates (oldest updated release first)
	ByDateAsc
	// ByDateDesc sorts by descending dates (latest updated release first)
	ByDateDesc
)

// Status is the status of a release
type Status string

// Describe the status of a release
// NOTE: Make sure to update cmd/helm/status.go when adding or modifying any of these statuses.
const (
	// StatusUnknown indicates that a release is in an uncertain state.
	StatusUnknown Status = "unknown"
	// StatusDeployed indicates that the release has been pushed to Kubernetes.
	StatusDeployed Status = "deployed"
	// StatusUninstalled indicates that a release has been uninstalled from Kubernetes.
	StatusUninstalled Status = "uninstalled"
	// StatusSuperseded indicates that this release object is outdated and a newer one exists.
	StatusSuperseded Status = "superseded"
	// StatusFailed indicates that the release was not successfully deployed.
	StatusFailed Status = "failed"
	// StatusUninstalling indicates that an uninstall operation is underway.
	StatusUninstalling Status = "uninstalling"
	// StatusPendingInstall indicates that an install operation is underway.
	StatusPendingInstall Status = "pending-install"
	// StatusPendingUpgrade indicates that an upgrade operation is underway.
	StatusPendingUpgrade Status = "pending-upgrade"
	// StatusPendingRollback indicates that a rollback operation is underway.
	StatusPendingRollback Status = "pending-rollback"
)

// ErrValuesSchema marks a failure to satisfy a chart's values.schema.json, as opposed to a failure
// of the operation itself. A consumer classifies with errors.Is and can then answer 4xx rather than
// 5xx: the input is invalid, and nothing about the server is wrong.
//
// A PLAIN SENTINEL, NOT helm's TYPE, and that is the same decision as Hook above. helm's
// chartutil.JSONSchemaValidationError would make helm.sh/helm/v3 a DIRECT requirement of every
// consumer that wants to classify this -- chart-inspector has it as indirect and should keep it
// that way. Detection needs helm and lives in helm/v3; the sentinel needs nothing and lives here.
//
// WHY A SENTINEL IS NEEDED AT ALL, measured against helm v3.20.2. helm HAS a typed error and
// discards it before any caller sees it:
//
//	chartutil/jsonschema.go:150  return JSONSchemaValidationError{err}   // typed, exported
//	chartutil/jsonschema.go:80   sb.WriteString(err.Error())             // type -> string
//	chartutil/jsonschema.go:107  return errors.New(sb.String())          // plain error
//	chartutil/values.go:168      fmt.Errorf(errFmt, err.Error())         // %s, not %w
//
// The chain is broken twice, and the render path uses the flattening entry point
// (chartutil/values.go:167). So errors.As on helm's own type cannot work downstream of an install,
// however correct it looks: the type exists and never arrives. Classifying by message text would
// mean depending on the format of a strings.Builder with no compatibility promise, which is the
// thing helm's wording has already changed under this repo once.
var ErrValuesSchema = errors.New("chart values do not satisfy the chart's values.schema.json")
