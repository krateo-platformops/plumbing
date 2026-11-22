package versionname

import (
	"os/exec"
	"strings"
	"testing"
)

// versionname exists so a binary can derive a version name without linking crdgen's dependencies.
// Keep it a leaf: every package it depends on must be from the standard library. CONTROL: the same
// query over crdgen must find non-standard packages, or the query cannot tell the two apart.
func TestVersionnameStaysStdlibOnly(t *testing.T) {
	nonStd := func(pkg string) []string {
		out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", pkg).Output()
		if err != nil {
			t.Fatalf("go list -deps %s: %v", pkg, err)
		}
		var got []string
		for _, l := range strings.Fields(string(out)) {
			if l != pkg {
				got = append(got, l)
			}
		}
		return got
	}
	if got := nonStd("github.com/krateo-platformops/plumbing/versionname"); len(got) != 0 {
		t.Fatalf("versionname must depend on the standard library only; it now pulls in %d package(s): %v", len(got), got)
	}
	if got := nonStd("github.com/krateo-platformops/plumbing/crdgen"); len(got) == 0 {
		t.Fatal("control: crdgen must have non-standard dependencies, or this query cannot see them")
	}
}
