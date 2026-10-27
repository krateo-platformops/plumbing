package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The property that matters: after the call, KUBECONFIG names a path that is NOT the default, so
// nothing a cluster tool does can reach ~/.kube/config.
func TestPinKubeconfig_PointsAwayFromTheDefault(t *testing.T) {
	before := os.Getenv("KUBECONFIG")
	t.Cleanup(func() { _ = os.Setenv("KUBECONFIG", before) })

	restore := PinKubeconfig("unit")
	defer restore()

	got := os.Getenv("KUBECONFIG")
	if got == "" {
		t.Fatal("KUBECONFIG is empty, so kind would fall back to ~/.kube/config — the thing this prevents")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if got == filepath.Join(home, ".kube", "config") {
			t.Fatalf("KUBECONFIG = %q, which IS the default config", got)
		}
	}
	if !strings.Contains(got, "krateo-unit-") {
		t.Errorf("KUBECONFIG = %q, want a path naming the suite so a leaked file is traceable", got)
	}
	if !strings.Contains(got, strconv.Itoa(os.Getpid())) {
		t.Errorf("KUBECONFIG = %q, want the pid in it so concurrent suites cannot collide", got)
	}
}

// Two suites running at once must not share a scratch file — one's teardown would delete the
// other's config mid-run.
func TestPinKubeconfig_DistinctPathsPerSuite(t *testing.T) {
	before := os.Getenv("KUBECONFIG")
	t.Cleanup(func() { _ = os.Setenv("KUBECONFIG", before) })

	r1 := PinKubeconfig("alpha")
	a := os.Getenv("KUBECONFIG")
	r1()
	r2 := PinKubeconfig("beta")
	b := os.Getenv("KUBECONFIG")
	r2()

	if a == b {
		t.Fatalf("both suites got %q; one suite's cleanup would delete the other's kubeconfig", a)
	}
}

// restore removes the scratch file rather than leaving one per run in TMPDIR.
func TestPinKubeconfig_RestoreRemovesTheScratchFile(t *testing.T) {
	before := os.Getenv("KUBECONFIG")
	t.Cleanup(func() { _ = os.Setenv("KUBECONFIG", before) })

	restore := PinKubeconfig("cleanup")
	path := os.Getenv("KUBECONFIG")
	if err := os.WriteFile(path, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatalf("seeding the scratch file: %v", err)
	}
	restore()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("scratch file still present after restore (stat err = %v)", err)
	}
}

// Control arm: restore must not touch the REAL kubeconfig path, whatever it is. A cleanup that
// removed the default config would be far worse than the bug being fixed.
func TestPinKubeconfig_RestoreNeverTargetsTheDefault(t *testing.T) {
	before := os.Getenv("KUBECONFIG")
	t.Cleanup(func() { _ = os.Setenv("KUBECONFIG", before) })

	restore := PinKubeconfig("safety")
	path := os.Getenv("KUBECONFIG")
	restore()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to compare against")
	}
	def := filepath.Join(home, ".kube", "config")
	if path == def {
		t.Fatalf("the scratch path WAS the default config (%q); restore would have deleted it", def)
	}
}
