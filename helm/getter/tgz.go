package getter

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
)

var _ Getter = (*tgzGetter)(nil)

type tgzGetter struct{}

func (g *tgzGetter) Get(ctx context.Context, opts GetOptions) (io.Reader, string, error) {
	if !isTGZ(opts.URI) {
		return nil, "", fmt.Errorf("%w: is not a valid tgz uri '%s'", ErrInvalidRepoRef, opts.URI)
	}

	dat, err := fetch(ctx, opts)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch tgz from uri %s: %w", opts.URI, err)
	}

	return dat, opts.URI, nil
}

// getSigned fetches the chart and <uri>.prov beside it, as a chart repository or `helm package
// --sign` lays them out. The provenance signs the chart under the URL's file name, which is the
// name `helm pull --verify` saves it under.
func (g *tgzGetter) getSigned(ctx context.Context, opts GetOptions) (*signedChart, error) {
	if !isTGZ(opts.URI) {
		return nil, fmt.Errorf("%w: is not a valid tgz uri '%s'", ErrInvalidRepoRef, opts.URI)
	}
	return fetchSignedHTTP(ctx, opts, opts.URI)
}

// fetchSignedHTTP fetches a chart at chartURI and its provenance at chartURI + ".prov", with the
// credentials and TLS settings of opts.
func fetchSignedHTTP(ctx context.Context, opts GetOptions, chartURI string) (*signedChart, error) {
	u, err := url.Parse(chartURI)
	if err != nil {
		return nil, fmt.Errorf("invalid chart url: %w", err)
	}
	o := opts
	o.URI = chartURI
	chart, err := fetchBytes(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch chart from %s: %w", chartURI, err)
	}
	o.URI = chartURI + ".prov"
	prov, err := fetchBytes(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("%w: fetching %s.prov: %v", ErrProvenance, path.Base(u.Path), err)
	}
	return &signedChart{chart: chart, prov: prov, fileName: path.Base(u.Path), resolvedURI: chartURI}, nil
}

func isTGZ(url string) bool {
	return strings.HasSuffix(url, ".tgz") || strings.HasSuffix(url, ".tar.gz")
}
