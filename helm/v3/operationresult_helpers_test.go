package helm

import helmconfig "github.com/krateo-platformops/plumbing/helm"

// releaseOf unwraps an Install or Upgrade result to its release, for the tests written before those
// operations returned an OperationResult (plumbing#135), which read only the release.
func releaseOf(op *helmconfig.OperationResult, err error) (*helmconfig.Release, error) {
	if op == nil {
		return nil, err
	}
	return op.Release, err
}
