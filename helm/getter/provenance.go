package getter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/provenance"
)

// signedChart is a chart fetched together with its provenance file, from the same source in the
// same request sequence, so the two describe the same bytes (a moving OCI tag cannot pair one
// version's chart with another's signature).
type signedChart struct {
	chart       []byte
	prov        []byte
	fileName    string // the file name the provenance signs the chart under
	resolvedURI string
}

// provenanceGetter is implemented by every getter that can serve Verify: it returns the chart and
// its provenance together. A getter without it cannot verify, and Get refuses rather than skip.
type provenanceGetter interface {
	getSigned(ctx context.Context, opts GetOptions) (*signedChart, error)
}

// verifyProvenance checks prov against the keyring and chart against the sha256 prov signs for
// fileName. It is helm's own check (provenance.Signatory.Verify, what `helm pull --verify` runs),
// fed from a private temporary directory because helm reads both from files; nothing about the
// signature or the digest is reimplemented here.
func verifyProvenance(chart, prov []byte, fileName, keyring string) error {
	if keyring == "" {
		return ErrNoKeyring
	}
	sig, err := provenance.NewFromKeyring(keyring, "")
	if err != nil {
		return fmt.Errorf("%w: loading the keyring: %v", ErrProvenance, err)
	}
	dir, err := os.MkdirTemp("", "plumbing-provenance-*")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProvenance, err)
	}
	defer os.RemoveAll(dir)
	chartPath := filepath.Join(dir, filepath.Base(fileName))
	if err := os.WriteFile(chartPath, chart, 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrProvenance, err)
	}
	if err := os.WriteFile(chartPath+".prov", prov, 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrProvenance, err)
	}
	if _, err := sig.Verify(chartPath, chartPath+".prov"); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrProvenance, filepath.Base(fileName), err)
	}
	return nil
}

// ociChartFileName is the name helm signs an OCI chart under: `helm package --sign` writes
// <name>-<version>.tgz and its .prov, and `helm pull --verify` saves the pulled chart under that
// name before verifying it. The name and version come from the chart's own Chart.yaml.
func ociChartFileName(chart []byte) (string, error) {
	ch, err := loader.LoadArchive(bytes.NewReader(chart))
	if err != nil {
		return "", fmt.Errorf("%w: reading the chart's name and version: %v", ErrProvenance, err)
	}
	return ch.Metadata.Name + "-" + ch.Metadata.Version + ".tgz", nil
}

// getVerified fetches a chart and its provenance from the source, or both from the cache, and
// returns the chart only once it verifies. A cached chart is verified on every load, so neither a
// cache entry written without Verify nor one altered on disk is returned unverified; a cached chart
// without a cached provenance is fetched again with it, never paired with a provenance fetched
// later.
func getVerified(ctx context.Context, g Getter, o GetOptions, key string) (io.Reader, string, error) {
	if o.Keyring == "" {
		return nil, "", ErrNoKeyring
	}
	provKey := key + "\x00prov"
	nameKey := key + "\x00provname"
	if o.Cache != nil {
		if sc, ok := cachedSigned(o.Cache, key, provKey, nameKey, o.Version); ok {
			if err := verifyProvenance(sc.chart, sc.prov, sc.fileName, o.Keyring); err != nil {
				return nil, "", err
			}
			return bytes.NewReader(sc.chart), o.Version, nil
		}
	}
	pg, ok := g.(provenanceGetter)
	if !ok {
		return nil, "", fmt.Errorf("%w: %T cannot fetch provenance", ErrProvenance, g)
	}
	sc, err := pg.getSigned(ctx, o)
	if err != nil {
		return nil, "", fmt.Errorf("failed to get %s: %w", o.URI, err)
	}
	if err := verifyProvenance(sc.chart, sc.prov, sc.fileName, o.Keyring); err != nil {
		return nil, "", err
	}
	if o.Cache != nil {
		// Cached only once verified. The three entries are written together; a reader that finds
		// fewer than all three treats it as a miss.
		for _, e := range []struct {
			k string
			b []byte
		}{{key, sc.chart}, {provKey, sc.prov}, {nameKey, []byte(sc.fileName)}} {
			if err := o.Cache.Set(e.k, o.Version, bytes.NewReader(e.b)); err != nil {
				return nil, "", fmt.Errorf("failed to cache %s: %w", o.URI, err)
			}
		}
	}
	return bytes.NewReader(sc.chart), sc.resolvedURI, nil
}

type cacheReader interface {
	Get(uri, version string) (io.ReadCloser, bool)
}

func cachedSigned(c cacheReader, key, provKey, nameKey, version string) (*signedChart, bool) {
	read := func(k string) ([]byte, bool) {
		rc, ok := c.Get(k, version)
		if !ok {
			return nil, false
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, MaxResponseSize))
		return b, err == nil
	}
	chart, ok1 := read(key)
	prov, ok2 := read(provKey)
	name, ok3 := read(nameKey)
	if !ok1 || !ok2 || !ok3 || len(name) == 0 {
		return nil, false
	}
	return &signedChart{chart: chart, prov: prov, fileName: string(name)}, true
}

// fetchBytes reads a fetch result fully.
func fetchBytes(ctx context.Context, opts GetOptions) ([]byte, error) {
	r, err := fetch(ctx, opts)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}
