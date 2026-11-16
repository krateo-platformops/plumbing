//go:build integration
// +build integration

package helm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/helm/getter"
	"github.com/stretchr/testify/require"
)

// frontendCRDsCommit is the krateoplatformops/frontend-chart commit testdata/vendored/frontend-crds was
// copied from (crd-chart/templates).
const frontendCRDsCommit = "e83c0ed5750548de41ab7550c34a3799a8cc43d5"

// TestLiveFetch_VendoredChartsMatchUpstream is the ONLY test in this suite that reaches the internet
// (plumbing#96). It fetches each vendored chart from the source the suite used before vendoring,
// through plumbing's own getters, and requires the bytes to be the vendored ones. So it answers two
// questions at once: does fetching from each kind of real source still work, and is the vendored
// copy still what upstream serves. A GitHub, Docker Hub or chart-repository outage fails THIS test,
// by name, and no other.
func TestLiveFetch_VendoredChartsMatchUpstream(t *testing.T) {
	verifyVendored(t)
	for _, tc := range []struct {
		name, uri, repo, version, file string
	}{
		{"OCI: bitnami nginx", "oci://registry-1.docker.io/bitnamicharts/nginx", "", "25.2.14", "nginx-25.2.14.tgz"},
		{"repo: ingress-nginx", "https://kubernetes.github.io/ingress-nginx", "ingress-nginx", "4.10.0", "ingress-nginx-4.10.0.tgz"},
		{"tgz: ingress-nginx release asset", "https://github.com/kubernetes/ingress-nginx/releases/download/helm-chart-4.10.0/ingress-nginx-4.10.0.tgz", "", "4.10.0", "ingress-nginx-4.10.0.tgz"},
		{"repo: redis-operator", "https://ot-container-kit.github.io/helm-charts", "redis-operator", "0.27.0", "redis-operator-0.27.0.tgz"},
		{"repo: cloudnative-pg", "https://cloudnative-pg.github.io/charts", "cloudnative-pg", "0.27.1", "cloudnative-pg-0.27.1.tgz"},
		{"repo: cnpg cluster", "https://cloudnative-pg.github.io/charts", "cluster", "0.5.0", "cluster-0.5.0.tgz"},
		{"repo: snowplow", "https://charts.krateo.io", "snowplow", "0.20.6", "snowplow-0.20.6.tgz"},
		{"repo: full-stack-app", "https://marketplace.krateo.io", "full-stack-app", "0.0.15", "full-stack-app-0.0.15.tgz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			opts := []getter.Option{getter.WithVersion(tc.version)}
			if tc.repo != "" {
				opts = append(opts, getter.WithRepo(tc.repo))
			}
			r, _, err := getter.Get(ctx, tc.uri, opts...)
			require.NoError(t, err, "live fetch of %s", tc.uri)
			h := sha256.New()
			_, err = io.Copy(h, r)
			require.NoError(t, err)
			want, err := fileSHA256(filepath.Join(vendoredChartDir, tc.file))
			require.NoError(t, err)
			require.Equal(t, want, hex.EncodeToString(h.Sum(nil)),
				"upstream no longer serves the vendored %s; re-vendor it deliberately (SOURCES.md)", tc.file)
		})
	}

	t.Run("git: frontend-chart CRDs", func(t *testing.T) {
		dir := t.TempDir()
		for _, args := range [][]string{
			{"init", "-q", dir},
			{"-C", dir, "fetch", "-q", "--depth", "1", "https://github.com/krateoplatformops/frontend-chart.git", frontendCRDsCommit},
			{"-C", dir, "checkout", "-q", "FETCH_HEAD"},
		} {
			out, err := exec.Command("git", args...).CombinedOutput()
			require.NoError(t, err, "git %v: %s", args, out)
		}
		entries, err := os.ReadDir(frontendCRDsDir)
		require.NoError(t, err)
		for _, e := range entries {
			if e.Name() == "SHA256SUMS" {
				continue
			}
			want, err := fileSHA256(filepath.Join(frontendCRDsDir, e.Name()))
			require.NoError(t, err)
			got, err := fileSHA256(filepath.Join(dir, "crd-chart", "templates", e.Name()))
			require.NoError(t, err)
			require.Equal(t, want, got, "frontend-chart@%s no longer has the vendored %s", frontendCRDsCommit[:7], e.Name())
		}
	})
}
