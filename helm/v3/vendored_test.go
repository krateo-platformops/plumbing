//go:build integration
// +build integration

package helm

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
)

// # Vendored prerequisite charts (plumbing#96)
//
// The integration suite used to fetch every chart it installs from the internet: bitnami's OCI
// registry, kubernetes.github.io, a GitHub release asset, ot-container-kit, cloudnative-pg,
// charts.krateo.io, marketplace.krateo.io and a git clone. Its result then depended on all of them
// being up, and during a GitHub release-download outage (2026-11-08, 504s) it failed on a docs-only PR.
// A red check could not say whether the PR broke anything.
//
// The charts now live in testdata/vendored, pinned by sha256 (SOURCES.md says where each came from).
// Each test serves them from a server INSIDE the test, of the same kind it used to reach: an HTTP chart
// repository with an index, an OCI registry, a plain .tgz URL. So the repo, OCI and URL fetch paths are
// still exercised end to end against real protocol servers (decided by Diego, 2026-11-16), and only
// TestLiveFetch_VendoredChartsMatchUpstream reaches the internet: an outage fails that one test, by
// name, and nothing else.
//
// What this does NOT cover: the cluster still pulls container IMAGES from their registries. That is
// a different dependency, and not the one #96 measured failing.

const (
	vendoredDir      = "testdata/vendored"
	vendoredChartDir = vendoredDir + "/charts"
	frontendCRDsDir  = vendoredDir + "/frontend-crds"
)

var (
	verifyVendoredOnce sync.Once
	vendoredProblems   []string
)

// verifyVendored fails the test if any vendored file differs from its pinned sha256, or if a file is
// present that is not pinned. The files are checked once per binary, and every caller sees the result.
func verifyVendored(t *testing.T) {
	t.Helper()
	verifyVendoredOnce.Do(func() {
		for _, dir := range []string{vendoredChartDir, frontendCRDsDir} {
			vendoredProblems = append(vendoredProblems, checkSums(dir)...)
		}
	})
	require.Empty(t, vendoredProblems, "vendored testdata does not match its pinned checksums")
}

func checkSums(dir string) []string {
	f, err := os.Open(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return []string{err.Error()}
	}
	defer f.Close()
	var problems []string
	pinned := map[string]bool{"SHA256SUMS": true}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			problems = append(problems, fmt.Sprintf("%s: malformed line %q", dir, sc.Text()))
			continue
		}
		want, name := fields[0], fields[1]
		pinned[name] = true
		got, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if got != want {
			problems = append(problems, fmt.Sprintf("%s/%s: sha256 %s, pinned %s", dir, name, got, want))
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return append(problems, err.Error())
	}
	for _, e := range entries {
		if !pinned[e.Name()] {
			problems = append(problems, fmt.Sprintf("%s/%s: not pinned in SHA256SUMS", dir, e.Name()))
		}
	}
	return problems
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// vendoredRepo serves every vendored chart as an HTTP chart repository, with an index.yaml helm builds
// from the archives themselves, and returns its URL.
func vendoredRepo(t *testing.T) string {
	t.Helper()
	verifyVendored(t)
	dir := t.TempDir()
	entries, err := os.ReadDir(vendoredChartDir)
	require.NoError(t, err)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tgz") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(vendoredChartDir, e.Name()))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600))
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	idx, err := repo.IndexDirectory(dir, srv.URL)
	require.NoError(t, err)
	require.NoError(t, idx.WriteFile(filepath.Join(dir, "index.yaml"), 0o600))
	return srv.URL
}

// vendoredTGZ returns a plain URL to one vendored archive, the shape of a GitHub release asset.
func vendoredTGZ(t *testing.T, file string) string {
	t.Helper()
	return vendoredRepo(t) + "/" + file
}

// vendoredOCI serves one vendored chart from an in-test OCI registry and returns its oci:// reference
// without a tag, as the tests used for bitnami's registry. The registry listens on 127.0.0.1, which
// plumbing's OCI getter reaches over plain HTTP.
//
// It is the PULL side of the OCI distribution API, written here rather than imported: the distribution
// registry helm's own tests use pulls in an OpenTelemetry log exporter (otlploggrpc v0.8.0) that does
// not compile against the otel v1.47 plumbing requires (measured on #96). Pull is all the getter
// does, and every route is strict: an unknown repository, tag or digest is a 404, so a getter that
// asked for the wrong thing fails here instead of being served anyway.
func vendoredOCI(t *testing.T, repoPath, file string) string {
	t.Helper()
	verifyVendored(t)

	layer, err := os.ReadFile(filepath.Join(vendoredChartDir, file))
	require.NoError(t, err)
	ch, err := loader.LoadArchive(bytes.NewReader(layer))
	require.NoError(t, err, "loading %s", file)
	config, err := json.Marshal(ch.Metadata)
	require.NoError(t, err)

	manifest, err := json.Marshal(ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: registry.ConfigMediaType, Digest: digest.FromBytes(config), Size: int64(len(config))},
		Layers:    []ocispec.Descriptor{{MediaType: registry.ChartLayerMediaType, Digest: digest.FromBytes(layer), Size: int64(len(layer))}},
	})
	require.NoError(t, err)
	tag := ch.Metadata.Version
	blobs := map[string][]byte{
		digest.FromBytes(config).String(): config,
		digest.FromBytes(layer).String():  layer,
	}
	manifestDigest := digest.FromBytes(manifest).String()

	prefix := "/v2/" + repoPath + "/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "pull only", http.StatusMethodNotAllowed)
			return
		}
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == prefix+"tags/list":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"name": repoPath, "tags": []string{tag}})
		case r.URL.Path == prefix+"manifests/"+tag || r.URL.Path == prefix+"manifests/"+manifestDigest:
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			w.Header().Set("Content-Length", fmt.Sprint(len(manifest)))
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		case strings.HasPrefix(r.URL.Path, prefix+"blobs/"):
			b, ok := blobs[strings.TrimPrefix(r.URL.Path, prefix+"blobs/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			if r.Method == http.MethodGet {
				_, _ = w.Write(b)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return fmt.Sprintf("oci://%s/%s", strings.TrimPrefix(srv.URL, "http://"), repoPath)
}

// The checksum gate catches the two ways vendored testdata can drift: a pinned file whose bytes
// changed, and a file added without a pin. The control, an intact directory, passes.
func TestVendoredChecksumsDetectDrift(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("a.tgz", "original")
	sum := sha256.Sum256([]byte("original"))
	write("SHA256SUMS", hex.EncodeToString(sum[:])+"  a.tgz\n")
	require.Empty(t, checkSums(dir), "an intact directory must pass")

	write("a.tgz", "tampered")
	require.Len(t, checkSums(dir), 1, "a changed file must be reported")

	write("a.tgz", "original")
	write("b.tgz", "unpinned")
	require.Len(t, checkSums(dir), 1, "an unpinned file must be reported")
}
