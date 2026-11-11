package e2e

import "github.com/krateo-platformops/plumbing/e2e/kubepin"

// PinKubeconfig points KUBECONFIG at a scratch file and returns a cleanup
// function, so a test suite that starts a cluster can never reach the
// developer's ~/.kube/config. See kubepin.Pin, which it calls, for why and how;
// a package that e2e imports (endpoints, signup) calls kubepin.Pin directly.
func PinKubeconfig(suite string) (restore func()) { return kubepin.Pin(suite) }
