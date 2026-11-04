package helm

import (
	"context"
	"fmt"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/plumbing/helm/getter"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
)

const (
	suppportedChartType = "application"
)

func (c *client) loadChart(ctx context.Context, chartRef string, opts []getter.Option) (*chart.Chart, error) {
	chartReader, _, err := getter.Get(ctx, chartRef, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to get chart from %s: %w", chartRef, err)
	}

	ch, err := loader.LoadArchive(chartReader)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}

	return ch, nil
}

func (c *client) checkDependencies(ctx context.Context, ch *chart.Chart, chartRef string, cfg *helmconfig.ActionConfig) (*chart.Chart, error) {
	if ch.Metadata.Dependencies == nil {
		return ch, nil
	}

	if err := action.CheckDependencies(ch, ch.Metadata.Dependencies); err != nil {
		if !cfg.DependencyUpdate {
			return nil, fmt.Errorf("missing dependencies: %w", err)
		}
		// Reload chart with updated dependencies
		reloaded, err := c.loadChart(ctx, chartRef, c.buildGetterOpts(cfg))
		if err != nil {
			return nil, fmt.Errorf("failed to reload chart after dependency update: %w", err)
		}
		return reloaded, nil
	}
	return ch, nil
}

func checkChartType(ch *chart.Chart) error {
	if ch.Metadata.Type != suppportedChartType && ch.Metadata.Type != "" {
		return fmt.Errorf("chart type mismatch: expected %s, got %s", suppportedChartType, ch.Metadata.Type)
	}
	return nil
}

func toWrapperRelease(rel *release.Release) *helmconfig.Release {
	r := &helmconfig.Release{
		Name:         rel.Name,
		Namespace:    rel.Namespace,
		Revision:     rel.Version,
		ChartVersion: rel.Chart.Metadata.Version,
		Status:       helmconfig.Status(rel.Info.Status),
		Manifest:     rel.Manifest,
	}
	if rel.Info != nil {
		r.Updated = rel.Info.LastDeployed.Time // helm's pkg/time.Time -> stdlib time.Time
	}
	// Hooks are carried because helm keeps them OUT of Manifest, so a consumer reading only
	// Manifest cannot see them (core-provider#311: no RBAC was ever generated for a hook object,
	// which made a pre-delete hook's uninstall impossible). Projected into plumbing's own Hook so
	// a consumer does not take a direct dependency on helm's release package.
	//
	// Events are copied rather than aliased: helm's HookEvent is a distinct string type, and
	// returning it would put helm in a consumer's import graph for a field they read as text.
	for _, h := range rel.Hooks {
		if h == nil {
			continue
		}
		ev := make([]string, 0, len(h.Events))
		for _, e := range h.Events {
			ev = append(ev, string(e))
		}
		r.Hooks = append(r.Hooks, helmconfig.Hook{
			Name:     h.Name,
			Kind:     h.Kind,
			Path:     h.Path,
			Manifest: h.Manifest,
			Events:   ev,
		})
	}
	return r
}
