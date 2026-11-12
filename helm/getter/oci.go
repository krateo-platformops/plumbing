package getter

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/krateo-platformops/plumbing/dialguard"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

var _ Getter = (*ociGetter)(nil)

// Constants defined by Helm OCI support
const (
	ChartLayerMediaType  = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	LegacyLayerMediaType = "application/tar+gzip" // Fallback for legacy registries
)

// Shared Transport to enable Connection Pooling.
//
// It is dialguard's Transport (core-provider#306 / snowplow#1131): the OCI path carries a chart URL
// from a CompositionDefinition's spec, so it is one of the destinations a user influences. Built by
// dialguard.Transport rather than as a literal with dialguard's dialer, so that on js/wasm it has no
// dial function and requests go through fetch (plumbing#138), as on every other plumbing client.
var sharedTransport = dialguard.Transport()

type ociGetter struct{}

func NewOCIGetter() Getter {
	return &ociGetter{}
}

func (g *ociGetter) Get(ctx context.Context, opts GetOptions) (io.Reader, string, error) {
	if !isOCI(opts.URI) {
		return nil, "", fmt.Errorf("uri '%s' is not a valid OCI ref", opts.URI)
	}

	// 1. Prepare URI (remove oci:// prefix)
	refString := strings.TrimPrefix(opts.URI, "oci://")
	if opts.Repo != "" {
		refString = fmt.Sprintf("%s/%s", refString, opts.Repo)
	}

	// Add tag/version if missing and not using digest
	if opts.Version != "" && !strings.Contains(refString, "@") {
		lastSlash := strings.LastIndex(refString, "/")
		afterLastSlash := refString
		if lastSlash != -1 {
			afterLastSlash = refString[lastSlash:]
		}
		if !strings.Contains(afterLastSlash, ":") {
			refString = fmt.Sprintf("%s:%s", refString, opts.Version)
		}
	}

	// 2. Create Remote Repository
	repo, err := remote.NewRepository(refString)
	if err != nil {
		return nil, "", fmt.Errorf("invalid repository reference: %w", err)
	}

	registry := repo.Reference.Registry
	repo.PlainHTTP = strings.HasPrefix(strings.ToLower(registry), "localhost") ||
		strings.HasPrefix(strings.ToLower(registry), "127.0.0.1") ||
		strings.HasPrefix(strings.ToLower(registry), "[::1]")

	// 3. Configure Auth Client FIRST (before any registry operations)
	authClient := &auth.Client{
		Client: &http.Client{
			// On js/wasm, fetch would follow a redirect Go never sees; RoundTripper refuses it there
			// (dialguard).
			Transport: dialguard.RoundTripper(getTransport(opts.InsecureSkipVerifyTLS)),
			Timeout:   opts.Timeout,
		},
		Cache: auth.NewCache(),
	}

	// Set credentials if provided
	if opts.Username != "" && opts.Password != "" {
		authClient.Credential = auth.StaticCredential(repo.Reference.Registry, auth.Credential{
			Username: opts.Username,
			Password: opts.Password,
		})
	}

	repo.Client = authClient

	// 4. Now handle version discovery if needed
	reference := repo.Reference.Reference
	if reference == "" || reference == "latest" {
		var tags []string
		err := repo.Tags(ctx, "", func(repoTags []string) error {
			tags = append(tags, repoTags...)
			return nil
		})
		if err != nil {
			return nil, "", fmt.Errorf("failed to list tags for discovery: %w", err)
		}

		latest, err := findBestSemverMatch(tags)
		if err != nil {
			return nil, "", fmt.Errorf("no valid semantic versions found: %w", err)
		}
		reference = latest
		refString = fmt.Sprintf("%s:%s", refString, latest)
	}

	// 5. Resolve Tag -> Descriptor
	desc, err := repo.Resolve(ctx, reference)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve reference %s: %w - %s", refString, err, reference)
	}

	// 6. Download Manifest
	// FetchAll is acceptable here as the manifest is small (KB), so loading it into RAM is safe.
	manifestBytes, err := content.FetchAll(ctx, repo, desc)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch manifest: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, "", fmt.Errorf("failed to parse manifest: %w", err)
	}

	// 7. Identify the Chart Layer
	var chartLayerDesc *ocispec.Descriptor
	for _, layer := range manifest.Layers {
		if layer.MediaType == ChartLayerMediaType || layer.MediaType == LegacyLayerMediaType {
			l := layer // Create local copy for pointer
			chartLayerDesc = &l
			break
		}
	}

	if chartLayerDesc == nil {
		// Fallback: if there is only one layer, assume it is the chart
		if len(manifest.Layers) == 1 {
			chartLayerDesc = &manifest.Layers[0]
		} else {
			return nil, "", fmt.Errorf("chart layer not found in manifest")
		}
	}

	// 8. Stream the Layer
	// repo.Fetch returns an io.ReadCloser connected directly to the HTTP response.
	// This does NOT load the chart data into memory.
	rc, err := repo.Fetch(ctx, *chartLayerDesc)
	if err != nil {
		return nil, "", fmt.Errorf("failed to start stream for layer %s: %w", chartLayerDesc.Digest, err)
	}

	return rc, "oci://" + refString, nil
}

// getTransport handles secure/insecure TLS while reusing the shared connection pool
func getTransport(insecure bool) http.RoundTripper {
	if insecure {
		// Shallow clone and modify TLS config
		t := sharedTransport.Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		return retry.NewTransport(t) // ORAS retry wrapper
	}
	return retry.NewTransport(sharedTransport)
}

func findBestSemverMatch(tags []string) (string, error) {
	var versions []*semver.Version
	for _, t := range tags {
		v, err := semver.NewVersion(t)
		if err != nil {
			continue // Skip non-semver tags
		}
		// By default, Helm ignores prereleases
		if v.Prerelease() == "" {
			versions = append(versions, v)
		}
	}

	if len(versions) == 0 {
		return "", fmt.Errorf("no stable semver tags found")
	}

	// Sort versions
	sort.Sort(semver.Collection(versions))

	// Return the last (highest) one
	return versions[len(versions)-1].Original(), nil
}
