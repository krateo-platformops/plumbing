package helm

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/helm/getter/cache"
)

// testCache gives one test client a chart cache of its own, in the test's temporary directory.
//
// WithCache with no options uses the process-wide default directory (os.TempDir()/helm-chart-cache, 24h
// TTL), keyed by chart URI and version. Tests serve their charts from httptest servers at
// http://127.0.0.1:<random port>/c-0.1.0.tgz, so a test whose server is given a port an earlier test
// used (in this run, or in any run on the same machine within 24h) is handed that earlier test's
// chart from the cache instead of its own. On main 9bc0ed7 that made
// TestEnvtest_SkipSchemaValidationSkipsBothChecks install a cached chart without values.schema.json
// and report "control: a schema-violating install must fail with ErrValuesSchema, got <nil>", while
// the identical tree had passed on the PR.
func testCache(t testing.TB) ClientOption {
	t.Helper()
	return WithCache(cache.WithDir(t.TempDir()))
}

// The rule above holds only if no test builds a client with the shared default. Every test file
// in this package is checked, whatever its build tag, because a build-tagged file is invisible to
// the compiler in an untagged run.
func TestNoTestClientUsesTheSharedDefaultChartCache(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no test files found; the check would pass without looking at anything")
	}
	bare := regexp.MustCompile(`\bWithCache\(\s*\)`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if bare.MatchString(line) {
				t.Errorf("%s:%d: WithCache with no options shares os.TempDir()/helm-chart-cache across tests; use testCache(t)", f, i+1)
			}
		}
	}
}
