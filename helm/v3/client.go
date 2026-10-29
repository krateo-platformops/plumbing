package helm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/helm/getter/cache"
	"github.com/krateo-platformops/plumbing/helm/v3/tracer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/storage/driver"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
)

// isRESTMappingMiss reports whether err is a "the REST mapper does not know this kind yet" failure —
// i.e. the object's CRD exists but is not (yet) in the discovery cache. helm surfaces this while
// building the manifest as a stringified "no matches for kind ... ensure CRDs are installed first" /
// "resource mapping not found", so a stale discovery cache (e.g. a composition CRD created moments
// earlier in the same bootstrap) wedges the whole render until the process is restarted. We match both
// the typed error (when %w-wrapped) and helm's stringified form.
func isRESTMappingMiss(err error) bool {
	if err == nil {
		return false
	}
	if meta.IsNoMatchError(err) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "no matches for kind") ||
		strings.Contains(s, "resource mapping not found") ||
		strings.Contains(s, "ensure CRDs are installed first")
}

var _ helmconfig.Client = (*client)(nil)

type client struct {
	settings          *cli.EnvSettings
	namespace         string
	restConfig        *rest.Config
	cache             *cache.DiskCache
	debugLog          action.DebugLog
	cachedClients     *CachedClients
	crdInformerCancel context.CancelFunc
}

type ClientOption func(*client) error

func NewClient(cfg *rest.Config, opts ...ClientOption) (*client, error) {
	settings := cli.New()

	// Default configuration
	c := &client{
		settings:   settings,
		namespace:  settings.Namespace(),
		restConfig: rest.CopyConfig(cfg),
		debugLog: func(format string, v ...interface{}) {
			// Default to discard if no logger is provided
		},
	}

	// Apply functional options
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	// Sanity check: fail fast at construction if the storage driver / discovery
	// can't be initialized. The actionConfig itself is discarded; every operation
	// builds its own via newActionConfig to keep the client safe under concurrency.
	probe := new(action.Configuration)
	driver := os.Getenv("HELM_DRIVER")
	probeLog := func(format string, v ...interface{}) {
		slog.Debug(fmt.Sprintf(format, v...))
	}
	var probeGetter *RESTClientGetter
	if c.cachedClients != nil {
		probeGetter = NewRESTClientGetterWithCachedClients(c.namespace, nil, c.restConfig, c.cachedClients)
	} else {
		probeGetter = NewRESTClientGetter(c.namespace, nil, c.restConfig)
	}
	if err := probe.Init(probeGetter, c.namespace, driver, probeLog); err != nil {
		return nil, fmt.Errorf("failed to init action config: %w", err)
	}

	return c, nil
}

// WithNamespace sets a custom namespace for the client.
func WithNamespace(ns string) ClientOption {
	return func(c *client) error {
		c.namespace = ns
		c.settings.SetNamespace(ns)
		return nil
	}
}

// WithCache initializes a disk cache with the provided options.
func WithCache(opts ...cache.Option) ClientOption {
	return func(c *client) error {
		dCache, err := cache.NewDiskCache(opts...)
		if err != nil {
			return fmt.Errorf("failed to create cache: %w", err)
		}
		c.cache = dCache
		return nil
	}
}

// WithLogger configures a custom slog handler for the client.
func WithLogger(logger action.DebugLog) ClientOption {
	return func(c *client) error {
		if logger == nil {
			// Fallback to discard if nil is passed
			logger = func(format string, v ...interface{}) {}
		}

		c.debugLog = logger

		return nil
	}
}

// WithCachedClients makes this client reuse a caller-supplied discovery client and RESTMapper
// instead of building its own.
//
// WHY THIS EXISTS. Without it, a client whose cachedClients is nil falls to the uncached
// RESTClientGetter, whose ToRESTMapper builds a fresh DeferredDiscoveryRESTMapper — a full API
// discovery of every group, version and resource in the cluster. A caller that constructs a client
// per unit of work therefore pays that discovery per unit of work.
//
// Measured in krateo-core-provider#202: the composition-dynamic-controller builds two clients per
// Observe and reconciles every 60s, so a controller managing NOTHING spent ~38s of every 60s and
// allocated 5.19 TB over 16 hours, ~45% of it in NewDiscoveryRESTMapper. WithCRDInformer did
// populate cachedClients, but created them per client, so the cache died with the client — and it
// additionally starts a CRD informer goroutine, which is a cost the caller may not want.
//
// Build the CachedClients once per process with NewCachedClients and pass the same pointer to
// every client. Sharing is safe: DeferredDiscoveryRESTMapper guards its delegate with a mutex, and
// the memory-cached discovery client is likewise synchronised.
//
// Freshness is preserved without the informer: the client already calls
// cachedClients.mapper.Reset() when an operation fails on a REST-mapping miss, which is the
// targeted invalidation — it resets exactly when a lookup proves the cache stale, rather than on
// every CRD event in the cluster.
func WithCachedClients(cc *CachedClients) ClientOption {
	return func(c *client) error {
		if cc == nil {
			return fmt.Errorf("WithCachedClients: cached clients must not be nil")
		}
		c.cachedClients = cc
		return nil
	}
}

// WithCRDInformer enables automatic discovery cache invalidation when CustomResourceDefinitions change.
// This is useful for applications that need to handle dynamic CRD registration.
// The informer will start watching CRDs and reset the discovery cache on Add, Update, or Delete events.
func WithCRDInformer(cfg *rest.Config, resyncTime time.Duration) ClientOption {
	return func(c *client) error {
		// Create cached clients if not already created
		if c.cachedClients == nil {
			cachedClients, err := NewCachedClients(cfg)
			if err != nil {
				return fmt.Errorf("failed to create cached clients: %w", err)
			}
			c.cachedClients = &cachedClients
		}

		// Create a new context for the informer
		ctx, cancel := context.WithCancel(context.Background())
		c.crdInformerCancel = cancel

		// Initialize the API extensions client
		apiExtCli, err := apiextclient.NewForConfig(cfg)
		if err != nil {
			cancel()
			return fmt.Errorf("failed to create API extensions client: %w", err)
		}

		crdInformer := NewCRDInformer(resyncTime, apiExtCli, c.cachedClients.mapper, c.debugLog)
		if err := crdInformer.Start(ctx); err != nil {
			cancel()
			return fmt.Errorf("failed to start CRD informer: %w", err)
		}

		return nil
	}
}

func (c *client) Close() error {
	// Stop the CRD informer if it was started
	if c.crdInformerCancel != nil {
		c.crdInformerCancel()
	}

	if c.cache != nil {
		c.cache.Stop()
	}
	return nil
}

func (c *client) Install(ctx context.Context, releaseName string, chartRef string, cfg *helmconfig.InstallConfig) (*helmconfig.Release, error) {
	if cfg == nil {
		cfg = &helmconfig.InstallConfig{}
	}
	if cfg.ActionConfig == nil {
		cfg.ActionConfig = &helmconfig.ActionConfig{}
	}

	namespace := c.namespace
	if cfg.Namespace != "" {
		namespace = cfg.Namespace
	}

	restCfg := c.restConfig
	if cfg.RestConfig != nil {
		restCfg = cfg.RestConfig
	}

	actionConfig, err := c.newActionConfig(ctx, namespace, restCfg, cfg.ActionConfig)
	if err != nil {
		return nil, err
	}

	rel, err := c.install(ctx, namespace, releaseName, chartRef, cfg, actionConfig)
	if err != nil && c.cachedClients != nil && isRESTMappingMiss(err) {
		// A CRD referenced by the chart may have been created moments ago (e.g. a composition CRD
		// generated earlier in the same bootstrap) and not yet be in the discovery cache, so the render
		// fails to map its kind. The CRD informer normally invalidates the cache on the CRD add, but that
		// can race the render; refresh discovery explicitly and retry ONCE — now that the kind is
		// actually being resolved — so a stale cache no longer wedges the install until a process restart.
		c.cachedClients.mapper.Reset()
		if ac2, e2 := c.newActionConfig(ctx, namespace, restCfg, cfg.ActionConfig); e2 == nil {
			rel, err = c.install(ctx, namespace, releaseName, chartRef, cfg, ac2)
		}
	}
	return rel, err
}

func (c *client) Upgrade(ctx context.Context, releaseName, chartRef string, cfg *helmconfig.UpgradeConfig) (*helmconfig.Release, error) {
	if cfg == nil {
		cfg = &helmconfig.UpgradeConfig{}
	}
	if cfg.ActionConfig == nil {
		cfg.ActionConfig = &helmconfig.ActionConfig{}
	}

	actionConfig, err := c.newActionConfig(ctx, c.namespace, c.restConfig, cfg.ActionConfig)
	if err != nil {
		return nil, err
	}

	upgradeClient := action.NewUpgrade(actionConfig)
	applyUpgradeConfig(upgradeClient, c.namespace, cfg)
	upgradeClient.PostRenderer = withDuplicateResourceValidation(cfg.PostRenderer, actionConfig.KubeClient)

	chart, err := c.loadChart(ctx, chartRef, c.buildGetterOpts(cfg.ActionConfig))
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}

	if err := checkChartType(chart); err != nil {
		return nil, fmt.Errorf("chart type check failed: %w", err)
	}

	chart, err = c.checkDependencies(ctx, chart, chartRef, cfg.ActionConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to check dependencies: %w", err)
	}

	rel, err := upgradeClient.RunWithContext(ctx, releaseName, chart, cfg.Values)
	if err != nil {
		return nil, fmt.Errorf("upgrade failed: %w", err)
	}
	return toWrapperRelease(rel), nil
}

func (c *client) Uninstall(ctx context.Context, releaseName string, cfg *helmconfig.UninstallConfig) error {
	actionConfig, err := c.newActionConfig(ctx, c.namespace, c.restConfig, nil)
	if err != nil {
		return err
	}

	cmd := action.NewUninstall(actionConfig)
	applyUninstallConfig(cmd, cfg)

	_, err = cmd.Run(releaseName)
	if err != nil {
		return fmt.Errorf("uninstall failed: %w", err)
	}

	return nil
}

func (c *client) Rollback(ctx context.Context, releaseName string, cfg *helmconfig.RollbackConfig) (*helmconfig.Release, error) {
	actionConfig, err := c.newActionConfig(ctx, c.namespace, c.restConfig, nil)
	if err != nil {
		return nil, err
	}

	rollbackClient := action.NewRollback(actionConfig)
	applyRollbackConfig(rollbackClient, cfg)

	err = rollbackClient.Run(releaseName)
	if err != nil {
		return nil, fmt.Errorf("rollback failed: %w", err)
	}

	rel, err := c.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	if err != nil {
		return nil, fmt.Errorf("failed to get release after rollback: %w", err)
	}

	return rel, nil
}

func (c *client) install(ctx context.Context, namespace, releaseName, chartRef string, cfg *helmconfig.InstallConfig, actionConfig *action.Configuration) (*helmconfig.Release, error) {
	if actionConfig == nil {
		return nil, errors.New("action config is required")
	}

	installClient := action.NewInstall(actionConfig)
	applyInstallConfig(installClient, releaseName, namespace, cfg)
	installClient.PostRenderer = withDuplicateResourceValidation(cfg.PostRenderer, actionConfig.KubeClient)

	chart, err := c.loadChart(ctx, chartRef, c.buildGetterOpts(cfg.ActionConfig))
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}

	if err := checkChartType(chart); err != nil {
		return nil, fmt.Errorf("chart type check failed: %w", err)
	}

	chart, err = c.checkDependencies(ctx, chart, chartRef, cfg.ActionConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to check dependencies: %w", err)
	}

	rel, err := installClient.RunWithContext(ctx, chart, cfg.Values)
	if err != nil {
		return nil, fmt.Errorf("install failed: %w", err)
	}

	return toWrapperRelease(rel), nil
}

func (c *client) newActionConfig(ctx context.Context, namespace string, restCfg *rest.Config, actionCfg *helmconfig.ActionConfig) (*action.Configuration, error) {
	actionConfig := new(action.Configuration)
	driver := os.Getenv("HELM_DRIVER")

	debugLog := func(format string, v ...interface{}) {
		slog.Debug(fmt.Sprintf(format, v...))
	}
	if c.debugLog != nil {
		debugLog = c.debugLog
	}

	cfgToUse := restCfg
	if cfgToUse == nil {
		cfgToUse = c.restConfig
	}

	if actionCfg != nil && actionCfg.Verbose {
		cfgToUse = rest.CopyConfig(cfgToUse)
		t := tracer.NewTracer(ctx, true)
		if actionCfg.LogWriter != nil {
			t = t.WithWriter(actionCfg.LogWriter)
		}
		cfgToUse.Wrap(func(rt http.RoundTripper) http.RoundTripper {
			return t.WithRoundTripper(rt)
		})
	}

	var clientGetter *RESTClientGetter
	if c.cachedClients != nil {
		clientGetter = NewRESTClientGetterWithCachedClients(namespace, nil, cfgToUse, c.cachedClients)
	} else {
		clientGetter = NewRESTClientGetter(namespace, nil, cfgToUse)
	}
	if err := actionConfig.Init(clientGetter, namespace, driver, debugLog); err != nil {
		return nil, fmt.Errorf("failed to init action config: %w", err)
	}

	return actionConfig, nil
}

func (c *client) GetRelease(ctx context.Context, releaseName string, cfg *helmconfig.GetConfig) (*helmconfig.Release, error) {
	namespace := c.namespace
	if cfg != nil && cfg.Namespace != "" {
		namespace = cfg.Namespace
	}

	actionConfig, err := c.newActionConfig(ctx, namespace, c.restConfig, nil)
	if err != nil {
		return nil, err
	}

	getClient := action.NewGet(actionConfig)
	applyGetConfig(getClient, cfg)

	rel, err := getClient.Run(releaseName)
	if err != nil {
		if errors.Is(err, driver.ErrReleaseNotFound) {
			return nil, nil // Not found is not an error for GetRelease
		}
		return nil, fmt.Errorf("get release failed: %w", err)
	}

	return toWrapperRelease(rel), nil
}

func (c *client) ListReleases(ctx context.Context, cfg *helmconfig.ListConfig) ([]*helmconfig.Release, error) {
	actionConfig, err := c.newActionConfig(ctx, c.namespace, c.restConfig, nil)
	if err != nil {
		return nil, err
	}

	listClient := action.NewList(actionConfig)
	applyListConfig(listClient, cfg)

	helmReleases, err := listClient.Run()
	if err != nil {
		return nil, fmt.Errorf("list releases failed: %w", err)
	}

	var releases []*helmconfig.Release
	for _, rel := range helmReleases {
		releases = append(releases, toWrapperRelease(rel))
	}

	return releases, nil
}
