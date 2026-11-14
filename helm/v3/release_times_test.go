package helm

import (
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	helmtime "helm.sh/helm/v3/pkg/time"
)

// core-provider#497: an uninstalling revision's clock is Info.Deleted, which helm stamps when the
// uninstall begins; LastDeployed stays at the deploy. A consumer that measured an uninstall from
// Updated measured from the deploy: a release deployed hours ago read as stuck the moment its
// uninstall started, and a record written at the uninstall's start never matched the revision.
// Both timestamps are carried, each as helm wrote it, and they are distinct values here so a mix-up
// cannot pass.
func TestToWrapperRelease_CarriesDeployAndUninstallTimes(t *testing.T) {
	deployed := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	uninstallStarted := deployed.Add(3 * time.Hour)
	src := &release.Release{
		Name: "focus-1", Namespace: "demo-system", Version: 8,
		Chart: &chart.Chart{Metadata: &chart.Metadata{Version: "0.1.0"}},
		Info: &release.Info{
			Status:       release.StatusUninstalling,
			LastDeployed: helmtime.Time{Time: deployed},
			Deleted:      helmtime.Time{Time: uninstallStarted},
		},
	}
	got := toWrapperRelease(src)
	if !got.Updated.Equal(deployed) {
		t.Errorf("Updated = %s, want the deploy time %s (Info.LastDeployed)", got.Updated, deployed)
	}
	if !got.Deleted.Equal(uninstallStarted) {
		t.Errorf("Deleted = %s, want the uninstall start %s (Info.Deleted)", got.Deleted, uninstallStarted)
	}

	// A revision never uninstalled carries a zero Deleted.
	src.Info = &release.Info{Status: release.StatusDeployed, LastDeployed: helmtime.Time{Time: deployed}}
	if got := toWrapperRelease(src); !got.Deleted.IsZero() {
		t.Errorf("Deleted = %s on a deployed revision, want zero", got.Deleted)
	}
}
