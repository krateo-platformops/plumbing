package helm

import (
	"bytes"
	"context"
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
}

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
	Install              bool
	MaxHistory           int
	ResetValues          bool
	ReuseValues          bool
	ResetThenReuseValues bool
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
	// The default is ListDeployed
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
