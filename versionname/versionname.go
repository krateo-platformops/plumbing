// Package versionname derives Kubernetes CRD version names from arbitrary version strings.
//
// It is a leaf package (standard library only), so a binary that only needs to derive a version
// name, such as a controller looking up the CRD version it serves, does not link crdgen and its
// CEL/ANTLR dependencies. crdgen.NormalizeVersionName delegates here, so the two cannot disagree.
package versionname

import (
	"regexp"
	"strings"
)

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

// Normalize converts an arbitrary version string (e.g. an OAS info.version like "1.2.3") into the
// legal Kubernetes CRD version name crdgen emits for it (e.g. "v1-2-3"): lowercased, every run of
// non-alphanumerics replaced by '-', '-' trimmed from the ends, and "v" prefixed when the result
// starts with a digit. '-' is the only separator a CRD version name allows
// ([a-z]([-a-z0-9]*[a-z0-9])?).
func Normalize(ver string) string {
	ver = strings.ToLower(ver)
	ver = nonAlphaNum.ReplaceAllString(ver, "-")
	ver = strings.Trim(ver, "-")
	if len(ver) > 0 && ver[0] >= '0' && ver[0] <= '9' {
		ver = "v" + ver
	}
	return ver
}

// ForChart is the CRD version name core-provider serves for a Helm chart version: Normalize("v" +
// chartVersion). The unconditional "v" is historical and load-bearing: core-provider has always
// derived the version as "v" + the chart version before normalizing, so a chart versioned "v1.0.0"
// is served as "vv1-0-0". Every site that derives a composition's CRD version from a chart version
// (core-provider and composition-dynamic-controller) must use this, so that the name it derives is
// the one the CRD serves, and so that no existing definition's served version moves.
func ForChart(chartVersion string) string {
	return Normalize("v" + chartVersion)
}
