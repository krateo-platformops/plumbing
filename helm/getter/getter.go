package getter

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/krateo-platformops/plumbing/dialguard"
	"io"
	"net/http"
	"strings"
	"time"
)

const MaxResponseSize = 100 * 1024 * 1024 // 100MB

// Getter is an interface to support GET to the specified URI.
type Getter interface {
	// Get file content by url string. It returns the content as a byte slice, the absolute URI (repo+chart+version), and error if any.
	Get(ctx context.Context, opts GetOptions) (io.Reader, string, error)
}

func Get(ctx context.Context, uri string, opts ...Option) (io.Reader, string, error) {
	o := GetOptions{
		URI:     uri,
		Timeout: 60 * time.Second,
	}
	for _, opt := range opts {
		opt(&o)
	}

	key := cacheKey(o.URI, o.Repo)

	// Check cache first
	if o.Cache != nil {
		if data, ok := o.Cache.Get(key, o.Version); ok {
			return data, o.Version, nil
		}
	}

	if o.URI == "" {
		return nil, "", errors.New("URI is required")
	}
	var err error
	var g Getter
	if isOCI(o.URI) {
		g = &ociGetter{}
	} else if isTGZ(o.URI) {
		g = &tgzGetter{}
	} else if isHTTP(o.URI) {
		g = &repoGetter{}
	} else {
		return nil, "", fmt.Errorf("%w: uri '%s'", ErrNoHandler, o.URI)
	}

	b, resolvedURI, err := g.Get(ctx, o)
	if err != nil {
		return nil, "", fmt.Errorf("failed to get %s: %w", o.URI, err)
	}

	if o.Cache != nil {
		if err := o.Cache.Set(key, o.Version, b); err != nil {
			return nil, "", fmt.Errorf("failed to cache %s: %w", o.URI, err)
		}
		// Source reader may own network resources (OCI streams from repo.Fetch).
		// Close it: cache.Set has already drained the bytes onto disk.
		if c, ok := b.(io.Closer); ok {
			_ = c.Close()
		}
		// Re-open the entry from the cache so the caller receives a fresh,
		// readable stream regardless of whether b was seekable. This avoids the
		// silent empty-stream bug when b is a non-seekable OCI reader.
		cached, ok := o.Cache.Get(key, o.Version)
		if !ok {
			return nil, "", fmt.Errorf("cache evicted right after Set for %s", o.URI)
		}
		return cached, resolvedURI, nil
	}

	return b, resolvedURI, nil
}

// cacheKey combines the registry URI with the chart name (Repo) so that two
// requests sharing URI+Version but referencing different charts get distinct
// cache entries. The null byte separator cannot appear in either component.
func cacheKey(uri, repo string) string {
	if repo == "" {
		return uri
	}
	return uri + "\x00" + repo
}

func fetch(ctx context.Context, opts GetOptions) (io.Reader, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URI, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create http request for uri %s: %w", opts.URI, err)
	}

	// Always set credentials on initial request
	if opts.Username != "" && opts.Password != "" {
		req.SetBasicAuth(opts.Username, opts.Password)
	}

	client := newHTTPClient(opts)

	// Strip credentials on cross-domain redirects unless PassCredentialsAll is true
	if !opts.PassCredentialsAll {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", opts.URI, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch %s : %s", opts.URI, resp.Status)
	}

	size := int64(512)
	if resp.ContentLength > 0 {
		size = resp.ContentLength
	}

	data := make([]byte, 0, size)
	buf := bytes.NewBuffer(data)

	// Limit the reading to avoid memory exhaustion attacks
	limitedReader := io.LimitReader(resp.Body, MaxResponseSize)

	_, err = buf.ReadFrom(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	// Convert to *bytes.Reader to allow Seeking (ReadAt, Seek)
	return bytes.NewReader(buf.Bytes()), nil
}

func newHTTPClient(opts GetOptions) *http.Client {
	// THIS TRANSPORT HAD NO DialContext AT ALL, so it used the zero-value dialer and nothing
	// constrained the destination — on the path that fetches a chart from a URL written in a
	// CompositionDefinition's spec, which is core-provider#306's primary path. The guarded dialer
	// is installed here rather than a field being added to the old literal, so the omission cannot
	// recur silently.
	transport := dialguard.Transport()
	transport.DisableCompression = true

	if opts.InsecureSkipVerifyTLS {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	return &http.Client{
		// On js/wasm, fetch would follow a redirect without CheckRedirect's credential strip
		// below; RoundTripper refuses it there (dialguard).
		Transport: dialguard.RoundTripper(transport),
		Timeout:   opts.Timeout,
	}
}
func isOCI(url string) bool {
	return strings.HasPrefix(url, "oci://")
}
