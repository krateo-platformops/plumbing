package getter

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/helm/getter/cache"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/crypto/openpgp" //nolint:staticcheck // helm's provenance package uses this one
	"helm.sh/helm/v3/pkg/provenance"
)

// plumbing#72: Verify was accepted and ignored, so a chart's provenance was never checked. These
// tests sign a real chart with helm's own ClearSign under a throwaway key and fetch it through Get
// with WithVerify, from each source plumbing supports. Each refusal has a passing control with the
// same source, so a refusal cannot be a fixture that never verified.

type signer struct {
	keyring string // path to a keyring holding the public key
	keyfile string // path to the private key
}

func newSigner(t *testing.T, name string) signer {
	t.Helper()
	e, err := openpgp.NewEntity(name, "plumbing#72 test", name+"@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var priv, pub bytes.Buffer
	if err := e.SerializePrivate(&priv, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(&pub); err != nil {
		t.Fatal(err)
	}
	s := signer{keyring: filepath.Join(dir, "pubring.gpg"), keyfile: filepath.Join(dir, "secring.gpg")}
	if err := os.WriteFile(s.keyring, pub.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.keyfile, priv.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

// chartTgz is a minimal chart archive named name, at version.
func chartTgz(t *testing.T, name, version, extra string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for path, body := range map[string]string{
		name + "/Chart.yaml":        "apiVersion: v2\nname: " + name + "\nversion: " + version + "\n",
		name + "/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\ndata:\n  k: \"" + extra + "\"\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sign returns the .prov helm's own ClearSign writes for chart, saved under fileName.
func (s signer) sign(t *testing.T, chart []byte, fileName string) []byte {
	t.Helper()
	sig, err := provenance.NewFromFiles(s.keyfile, s.keyring)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), fileName)
	if err := os.WriteFile(p, chart, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := sig.ClearSign(p)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(out)
}

// serve answers each path in files with its body, and 404 for anything else.
func serve(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getBytes(t *testing.T, uri string, opts ...Option) ([]byte, error) {
	t.Helper()
	r, _, err := Get(context.Background(), uri, append([]Option{WithTimeout(10 * time.Second)}, opts...)...)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestVerify_TGZ(t *testing.T) {
	owner, stranger := newSigner(t, "owner"), newSigner(t, "stranger")
	chart := chartTgz(t, "mychart", "0.1.0", "a")
	prov := owner.sign(t, chart, "mychart-0.1.0.tgz")
	altered := chartTgz(t, "mychart", "0.1.0", "b")

	signed := serve(t, map[string][]byte{"/mychart-0.1.0.tgz": chart, "/mychart-0.1.0.tgz.prov": prov})
	got, err := getBytes(t, signed.URL+"/mychart-0.1.0.tgz", WithVerify(owner.keyring))
	if err != nil {
		t.Fatalf("control: a chart signed by a key in the keyring was refused: %v", err)
	}
	if !bytes.Equal(got, chart) {
		t.Fatal("control: the verified chart is not the chart served")
	}

	for _, tc := range []struct {
		name    string
		files   map[string][]byte
		keyring string
		want    error
	}{
		{"unsigned: no .prov", map[string][]byte{"/mychart-0.1.0.tgz": chart}, owner.keyring, ErrProvenance},
		{"altered after signing", map[string][]byte{"/mychart-0.1.0.tgz": altered, "/mychart-0.1.0.tgz.prov": prov}, owner.keyring, ErrProvenance},
		{"signed by a key not in the keyring", map[string][]byte{"/mychart-0.1.0.tgz": chart, "/mychart-0.1.0.tgz.prov": prov}, stranger.keyring, ErrProvenance},
		{"no keyring", map[string][]byte{"/mychart-0.1.0.tgz": chart, "/mychart-0.1.0.tgz.prov": prov}, "", ErrNoKeyring},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(t, tc.files)
			got, err := getBytes(t, srv.URL+"/mychart-0.1.0.tgz", WithVerify(tc.keyring))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v (chart returned: %v), want %v", err, got != nil, tc.want)
			}
		})
	}
	// Without Verify nothing is checked, as before: the unsigned chart is returned.
	unsigned := serve(t, map[string][]byte{"/mychart-0.1.0.tgz": chart})
	if _, err := getBytes(t, unsigned.URL+"/mychart-0.1.0.tgz"); err != nil {
		t.Fatalf("without Verify an unsigned chart must still load: %v", err)
	}
}

func TestVerify_Repository(t *testing.T) {
	owner := newSigner(t, "owner")
	chart := chartTgz(t, "mychart", "0.1.0", "a")
	prov := owner.sign(t, chart, "mychart-0.1.0.tgz")
	index := []byte("apiVersion: v1\nentries:\n  mychart:\n    - name: mychart\n      version: 0.1.0\n      urls: [charts/mychart-0.1.0.tgz]\n")

	signed := serve(t, map[string][]byte{"/index.yaml": index, "/charts/mychart-0.1.0.tgz": chart, "/charts/mychart-0.1.0.tgz.prov": prov})
	if _, err := getBytes(t, signed.URL, WithRepo("mychart"), WithVersion("0.1.0"), WithVerify(owner.keyring)); err != nil {
		t.Fatalf("control: a signed repository chart was refused: %v", err)
	}
	unsigned := serve(t, map[string][]byte{"/index.yaml": index, "/charts/mychart-0.1.0.tgz": chart})
	if _, err := getBytes(t, unsigned.URL, WithRepo("mychart"), WithVersion("0.1.0"), WithVerify(owner.keyring)); !errors.Is(err, ErrProvenance) {
		t.Fatalf("an unsigned repository chart: got %v, want ErrProvenance", err)
	}
}

// ociRegistry serves one manifest and its blobs for any repository name.
func ociRegistry(t *testing.T, layers map[string][]byte) *httptest.Server {
	t.Helper()
	blobs := map[string][]byte{}
	m := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: "application/vnd.cncf.helm.config.v1+json", Digest: digest.FromString("{}"), Size: 2},
	}
	blobs[digest.FromString("{}").String()] = []byte("{}")
	for _, mt := range []string{ChartLayerMediaType, ProvLayerMediaType} {
		b, ok := layers[mt]
		if !ok {
			continue
		}
		d := digest.FromBytes(b)
		blobs[d.String()] = b
		m.Layers = append(m.Layers, ocispec.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))})
	}
	mj, _ := json.Marshal(m)
	md := digest.FromBytes(mj)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Docker-Content-Digest", md.String())
			w.Header().Set("Content-Length", fmt.Sprint(len(mj)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(mj)
			}
		case strings.Contains(r.URL.Path, "/blobs/"):
			b, ok := blobs[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerify_OCI(t *testing.T) {
	owner := newSigner(t, "owner")
	chart := chartTgz(t, "mychart", "0.1.0", "a")
	// helm package --sign names the archive <name>-<version>.tgz and signs it under that name.
	prov := owner.sign(t, chart, "mychart-0.1.0.tgz")

	host := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	signed := ociRegistry(t, map[string][]byte{ChartLayerMediaType: chart, ProvLayerMediaType: prov})
	// localhost/127.0.0.1 registries are spoken to in plain HTTP (oci.go).
	if _, err := getBytes(t, "oci://"+host(signed)+"/charts/mychart", WithVersion("0.1.0"), WithVerify(owner.keyring)); err != nil {
		t.Fatalf("control: a signed OCI chart was refused: %v", err)
	}
	unsigned := ociRegistry(t, map[string][]byte{ChartLayerMediaType: chart})
	if _, err := getBytes(t, "oci://"+host(unsigned)+"/charts/mychart", WithVersion("0.1.0"), WithVerify(owner.keyring)); !errors.Is(err, ErrProvenance) {
		t.Fatalf("an OCI chart with no provenance layer: got %v, want ErrProvenance", err)
	}
	altered := ociRegistry(t, map[string][]byte{ChartLayerMediaType: chartTgz(t, "mychart", "0.1.0", "b"), ProvLayerMediaType: prov})
	if _, err := getBytes(t, "oci://"+host(altered)+"/charts/mychart", WithVersion("0.1.0"), WithVerify(owner.keyring)); !errors.Is(err, ErrProvenance) {
		t.Fatalf("an OCI chart altered after signing: got %v, want ErrProvenance", err)
	}
}

// The disk cache never returns a chart unverified: neither one cached by a Get without Verify, nor
// a verified one asked for again with a keyring that does not hold its signer.
func TestVerify_CacheDoesNotBypassVerification(t *testing.T) {
	owner, stranger := newSigner(t, "owner"), newSigner(t, "stranger")
	chart := chartTgz(t, "mychart", "0.1.0", "a")
	prov := owner.sign(t, chart, "mychart-0.1.0.tgz")

	t.Run("cached without Verify, then asked for with Verify from a source with no .prov", func(t *testing.T) {
		dc, err := cache.NewDiskCache(cache.WithDir(t.TempDir()), cache.WithTTL(time.Hour), cache.WithCleanupInterval(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		srv := serve(t, map[string][]byte{"/mychart-0.1.0.tgz": chart})
		uri := srv.URL + "/mychart-0.1.0.tgz"
		if _, err := getBytes(t, uri, WithCache(dc)); err != nil {
			t.Fatalf("control: caching without Verify: %v", err)
		}
		if _, err := getBytes(t, uri, WithCache(dc), WithVerify(owner.keyring)); !errors.Is(err, ErrProvenance) {
			t.Fatalf("a chart cached unverified was returned under Verify: got %v, want ErrProvenance", err)
		}
	})
	t.Run("cached verified, then asked for with another keyring", func(t *testing.T) {
		dc, err := cache.NewDiskCache(cache.WithDir(t.TempDir()), cache.WithTTL(time.Hour), cache.WithCleanupInterval(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		srv := serve(t, map[string][]byte{"/mychart-0.1.0.tgz": chart, "/mychart-0.1.0.tgz.prov": prov})
		uri := srv.URL + "/mychart-0.1.0.tgz"
		if _, err := getBytes(t, uri, WithCache(dc), WithVerify(owner.keyring)); err != nil {
			t.Fatalf("control: verified fetch: %v", err)
		}
		srv.Close() // only the cache can answer now
		if _, err := getBytes(t, uri, WithCache(dc), WithVerify(owner.keyring)); err != nil {
			t.Fatalf("control: the verified chart must load from the cache with the same keyring: %v", err)
		}
		if _, err := getBytes(t, uri, WithCache(dc), WithVerify(stranger.keyring)); !errors.Is(err, ErrProvenance) {
			t.Fatalf("a cached chart was returned under a keyring without its signer: got %v, want ErrProvenance", err)
		}
	})
}
