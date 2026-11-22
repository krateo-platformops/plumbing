package crdgen

import "github.com/krateo-platformops/plumbing/versionname"

// NormalizeVersionName converts an arbitrary version string (e.g. an OAS info.version like "1.2.3")
// into the legal Kubernetes CRD version name crdgen emits for it (e.g. "v1-2-3"). Callers deriving a
// GVK/GVR outside crdgen MUST use this so their version matches the generated CRD's version name.
// It delegates to versionname.Normalize, which binaries that only derive a name should import
// instead, to avoid linking crdgen's dependencies.
func NormalizeVersionName(ver string) string {
	return versionname.Normalize(ver)
}
