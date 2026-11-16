package getter

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/krateo-platformops/plumbing/helm/getter/repo"
)

var _ Getter = (*repoGetter)(nil)

type repoGetter struct{}

func (g *repoGetter) Get(ctx context.Context, opts GetOptions) (io.Reader, string, error) {
	newopts, err := g.resolve(ctx, opts)
	if err != nil {
		return nil, "", err
	}
	dat, err := fetch(ctx, newopts)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch chart from url %s: %w", newopts.URI, err)
	}
	return dat, newopts.URI, nil
}

// getSigned resolves the chart in the repository index and fetches it with the .prov beside it.
func (g *repoGetter) getSigned(ctx context.Context, opts GetOptions) (*signedChart, error) {
	newopts, err := g.resolve(ctx, opts)
	if err != nil {
		return nil, err
	}
	return fetchSignedHTTP(ctx, newopts, newopts.URI)
}

// resolve reads the repository's index.yaml and returns the options that fetch the chart: its
// absolute URL, name and version, with opts' credentials and TLS settings.
func (g *repoGetter) resolve(ctx context.Context, opts GetOptions) (GetOptions, error) {
	if !isHTTP(opts.URI) {
		return GetOptions{}, fmt.Errorf("%w: uri '%s'", ErrInvalidRepoRef, opts.URI)
	}

	bufReader, err := fetch(ctx, GetOptions{
		URI:                   fmt.Sprintf("%s/index.yaml", opts.URI),
		InsecureSkipVerifyTLS: opts.InsecureSkipVerifyTLS,
		Username:              opts.Username,
		Password:              opts.Password,
		PassCredentialsAll:    opts.PassCredentialsAll,
	})
	if err != nil {
		return GetOptions{}, fmt.Errorf("failed to fetch index.yaml from repo: %w", err)
	}

	idx, err := repo.Load(bufReader, opts.URI, opts.Logging)
	if err != nil {
		return GetOptions{}, fmt.Errorf("failed to load index.yaml from repo: %w", err)
	}

	res, err := idx.Get(opts.Repo, opts.Version)
	if err != nil {
		return GetOptions{}, fmt.Errorf("failed to get chart %s@%s from index: %w", opts.Repo, opts.Version, err)
	}
	if len(res.URLs) == 0 {
		return GetOptions{}, fmt.Errorf("no package url found in index @ %s/%s", res.Name, res.Version)
	}

	chartUrlStr := res.URLs[0]
	u, err := url.Parse(chartUrlStr)
	if err != nil {
		return GetOptions{}, fmt.Errorf("invalid chart url: %w", err)
	}

	// If the URL is relative, resolve it against the base repository URI
	if !u.IsAbs() {
		// Use URLJoin helper for proper path joining
		joined, err := repo.URLJoin(opts.URI, chartUrlStr)
		if err != nil {
			return GetOptions{}, fmt.Errorf("failed to join chart URL: %w", err)
		}
		u, err = url.Parse(joined)
		if err != nil {
			return GetOptions{}, fmt.Errorf("invalid joined chart URL: %w", err)
		}
	}

	// Final validation: Ensure we have a valid absolute URI with a scheme
	if u.Scheme == "" || u.Host == "" {
		return GetOptions{}, fmt.Errorf("invalid chart url: %s", u.String())
	}

	return GetOptions{
		URI:                   u.String(),
		Version:               res.Version,
		Repo:                  res.Name,
		InsecureSkipVerifyTLS: opts.InsecureSkipVerifyTLS,
		Username:              opts.Username,
		Password:              opts.Password,
		PassCredentialsAll:    opts.PassCredentialsAll,
	}, nil
}

func isHTTP(uri string) bool {
	return strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://")
}
