package helm

import (
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/helm/getter"
	"helm.sh/helm/v3/pkg/action"
)

func (c *client) buildGetterOpts(cfg *helmconfig.ActionConfig) []getter.Option {
	return []getter.Option{
		getter.WithVersion(cfg.ChartVersion),
		getter.WithRepo(cfg.ChartName),
		getter.WithCache(c.cache),
		getter.WithCredentials(cfg.Username, cfg.Password),
		getter.WithPassCredentialsAll(cfg.PassCredentialsAll),
		getter.WithInsecureSkipVerifyTLS(cfg.InsecureSkipTLSverify),
	}
}

func applyInstallConfig(client *action.Install, releaseName string, namespace string, cfg *helmconfig.InstallConfig) {
	client.ReleaseName = releaseName
	client.Namespace = namespace
	client.Version = cfg.ChartVersion
	client.Timeout = cfg.Timeout
	client.Wait = cfg.Wait
	client.Atomic = cfg.Atomic
	client.Force = cfg.Force
	client.GenerateName = cfg.GenerateName
	client.CreateNamespace = cfg.CreateNamespace
	client.Labels = cfg.Labels
	client.WaitForJobs = cfg.WaitForJobs
	client.DisableOpenAPIValidation = true // This has no real effect on Helm v3, it only return warnings https://github.com/helm/helm/pull/12502 (we disable to avoid unuseful resource consumption)
	client.Replace = cfg.Replace
	client.TakeOwnership = cfg.TakeOwnership
	client.PostRenderer = cfg.PostRenderer
	client.Description = cfg.Description
	client.DependencyUpdate = cfg.DependencyUpdate
	client.Devel = cfg.Devel
	client.IncludeCRDs = cfg.IncludeCRDs
	client.SubNotes = cfg.SubNotes
	client.HideNotes = cfg.HideNotes
	client.EnableDNS = cfg.EnableDNS
	client.PassCredentialsAll = cfg.PassCredentialsAll
	client.Username = cfg.Username
	client.Password = cfg.Password
	client.InsecureSkipTLSverify = cfg.InsecureSkipTLSverify

	applyDryRun(&client.DryRun, &client.DryRunOption, cfg.DryRun)
}

func applyUpgradeConfig(client *action.Upgrade, namespace string, cfg *helmconfig.UpgradeConfig) {
	client.Namespace = namespace
	client.Version = cfg.ChartVersion
	client.Timeout = cfg.Timeout
	client.Wait = cfg.Wait
	client.Atomic = cfg.Atomic
	client.Force = cfg.Force
	// Adopt pre-existing, un-owned resources when the caller opts in — mirrors applyInstallConfig.
	// Without this line cfg.TakeOwnership was silently dropped on every Upgrade, so Helm's ownership
	// gate (existingResourceConflict -> "invalid ownership metadata") kept firing even when the
	// caller explicitly asked to adopt, defeating the CDC's intended reconcile self-healing.
	client.TakeOwnership = cfg.TakeOwnership
	client.Install = cfg.Install
	client.MaxHistory = cfg.MaxHistory
	client.Labels = cfg.Labels
	client.WaitForJobs = cfg.WaitForJobs
	client.DisableOpenAPIValidation = true // This has no real effect on Helm v3, it only return warnings https://github.com/helm/helm/pull/12502 (we disable to avoid unuseful resource consumption)
	client.PostRenderer = cfg.PostRenderer
	client.Description = cfg.Description
	client.DependencyUpdate = cfg.DependencyUpdate
	client.Devel = cfg.Devel
	client.SubNotes = cfg.SubNotes
	client.HideNotes = cfg.HideNotes
	client.EnableDNS = cfg.EnableDNS
	client.PassCredentialsAll = cfg.PassCredentialsAll
	client.Username = cfg.Username
	client.Password = cfg.Password
	client.InsecureSkipTLSverify = cfg.InsecureSkipTLSverify

	applyDryRun(&client.DryRun, &client.DryRunOption, cfg.DryRun)
}

func applyRollbackConfig(client *action.Rollback, cfg *helmconfig.RollbackConfig) {
	client.Timeout = cfg.Timeout
	client.Wait = cfg.Wait
	client.WaitForJobs = cfg.WaitForJobs
	client.Version = cfg.ReleaseVersion
	client.Force = cfg.Force
	client.DisableHooks = cfg.DisableHooks
	client.Recreate = cfg.Recreate
	client.DryRun = cfg.DryRun
	client.CleanupOnFail = cfg.CleanupOnFail
	client.MaxHistory = cfg.MaxHistory
}

func applyUninstallConfig(client *action.Uninstall, cfg *helmconfig.UninstallConfig) {
	client.DisableHooks = cfg.DisableHooks
	client.DryRun = cfg.DryRun
	client.KeepHistory = cfg.KeepHistory
	client.Wait = cfg.Wait
	client.Timeout = cfg.Timeout
	client.IgnoreNotFound = cfg.IgnoreNotFound
	client.DeletionPropagation = cfg.DeletionPropagation
	client.Description = cfg.Description
}

func applyListConfig(client *action.List, cfg *helmconfig.ListConfig) {
	client.All = cfg.All
	client.AllNamespaces = cfg.AllNamespaces
	client.Sort = action.Sorter(cfg.Sort)
	client.ByDate = cfg.ByDate
	client.SortReverse = cfg.SortReverse
	// ZERO MEANS "NOT SPECIFIED", and overwriting unconditionally was a silent destroyer
	// (plumbing#74). action.NewList has already set ListDeployed|ListFailed; assigning cfg's zero
	// value replaced that with 0, and helm's filterStateMask skips EVERY release when the mask is 0
	// (pkg/action/list.go: `mask := l.StateMask & currentStatus; if mask == 0 { continue }`). So a
	// caller that simply did not set the field got an EMPTY SLICE AND A NIL ERROR.
	//
	// There is nothing a caller can check against that. And the plausible value is the destructive
	// one: on an adoption path "no releases exist" means install a second release beside the live
	// one (core-provider#303), and on a teardown path it means "nothing to clean up".
	//
	// Nobody was broken, because both known callers set the field explicitly — which is also why no
	// test covered this: the coverage gap and the absence of impact have the same cause, so it read
	// as safe.
	if cfg.StateMask != 0 {
		client.StateMask = action.ListStates(cfg.StateMask)
	}
	client.Limit = cfg.Limit
	client.Offset = cfg.Offset
	client.Filter = cfg.Filter
	client.Short = cfg.Short
	client.NoHeaders = cfg.NoHeaders
	client.TimeFormat = cfg.TimeFormat
}

func applyGetConfig(client *action.Get, cfg *helmconfig.GetConfig) {
	client.Version = cfg.Version
}

func applyDryRun(dryRun *bool, dryRunOption *string, mode helmconfig.DryRunMode) {
	switch mode {
	case helmconfig.DryRunClient:
		*dryRun = true
		*dryRunOption = "client"
	case helmconfig.DryRunServer:
		*dryRun = true
		*dryRunOption = "server"
	}
}
