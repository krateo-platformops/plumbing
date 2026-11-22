package versionname

import (
	"fmt"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.3": "v1-2-3", "1.0.0-RC1": "v1-0-0-rc1", "1.0.0+build.5": "v1-0-0-build-5",
		"2.0.0-Beta.2": "v2-0-0-beta-2", "1.0.0--rc": "v1-0-0-rc", "v1.0.0": "v1-0-0",
		"V1": "v1", "..1..": "v1", "v1alpha1": "v1alpha1", "": "",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// ForChart must equal the name core-provider has always served: Normalize of
// fmt.Sprintf("v%s", strings.ReplaceAll(chartVersion, ".", "-")), the expression core-provider used
// (through 2.13.122) in go/core-provider/internal/tools/chart/chart.go ChartGroupVersionKind and
// go/core-provider/internal/tools/chart/chartfs/gvr.go GroupVersionKind to build the chart's GVK,
// which crdgen then normalized into the served CRD version. CONTROL: plain Normalize moves
// every v/V-prefixed version, so the comparison can tell the two rules apart.
func TestForChart_PreservesTheHistoricalServedName(t *testing.T) {
	cores := []string{"0.0.1", "1.0.0", "10.20.30", "1.2.3-RC1", "1.2.3-rc.1", "1.2.3+build.5",
		"1.2.3-Beta.2+x.Y", "1.0.0--rc", "1.0.0-a_b", "1.2.3-0", "2.0.0-alpha-1"}
	checked, moved := 0, 0
	for _, p := range []string{"", "v", "V"} {
		for _, c := range cores {
			v := p + c
			historical := Normalize(fmt.Sprintf("v%s", strings.ReplaceAll(v, ".", "-")))
			if got := ForChart(v); got != historical {
				t.Errorf("ForChart(%q) = %q, want the historical served name %q", v, got, historical)
			}
			if Normalize(v) != historical {
				moved++
			}
			checked++
		}
	}
	if checked != 33 || moved != 22 {
		t.Fatalf("control: checked=%d (want 33), plain Normalize moved=%d (want 22, the v/V-prefixed rows)", checked, moved)
	}
	if got := ForChart("v1.0.0"); got != "vv1-0-0" {
		t.Fatalf(`ForChart("v1.0.0") = %q, want "vv1-0-0"`, got)
	}
}
