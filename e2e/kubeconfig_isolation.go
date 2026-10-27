package e2e

import (
	"fmt"
	"os"
	"path/filepath"
)

// PinKubeconfig points KUBECONFIG at a scratch file and returns a cleanup function, so a test
// suite that starts a cluster can never reach the developer's ~/.kube/config.
//
// Call it as the FIRST thing in TestMain, before any cluster is created:
//
//	func TestMain(m *testing.M) {
//	    restore := e2e.PinKubeconfig("my-suite")
//	    ...
//	    code := testenv.Run(m)
//	    restore()           // not deferred: os.Exit does not run defers
//	    os.Exit(code)
//	}
//
// WHY THIS EXISTS, because the damage is not the obvious one.
//
// `kind create cluster` writes to and switches ~/.kube/config unless KUBECONFIG points elsewhere,
// and e2e-framework's DestroyCluster then REMOVES that context again. So the visible result is not
// "switched to kind-whatever", which anyone would notice and undo — it is a CLEARED
// current-context. Whatever cluster the developer was pointed at, they are now pointed at nothing,
// and the next kubectl fails in a way that looks like a credentials problem.
//
// It happened on a real workstation on 2026-10-27 (krateo-platformops/core-provider), from a suite
// with no build tag, so a plain `go test ./...` did it. It cost another session a diagnosis and a
// restore.
//
// It survives review easily because it is invisible where it is tested: on an ephemeral CI runner
// there is no kubeconfig worth clobbering, so the suite passes and nothing is ever reported. The
// failure only exists on a machine someone works on.
//
// The restore function removes the scratch file. It deliberately does NOT put the old KUBECONFIG
// value back: the process is exiting, and attempting to restore invites the subtler bug of
// restoring it too early, while a cluster teardown is still running and still needs the pin.
func PinKubeconfig(suite string) (restore func()) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("krateo-%s-%d.kubeconfig", suite, os.Getpid()))
	if err := os.Setenv("KUBECONFIG", path); err != nil {
		// Failing loudly is right: carrying on would silently fall back to ~/.kube/config, which
		// is the exact outcome this guards against.
		fmt.Fprintf(os.Stderr, "e2e.PinKubeconfig(%q): %v\n", suite, err)
		os.Exit(1)
	}
	return func() { _ = os.Remove(path) }
}
