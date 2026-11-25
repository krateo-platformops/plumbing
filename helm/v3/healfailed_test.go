package helm

import (
	"strings"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// The heal reads the latest revision again, with its labels. When another writer added one after
// Reconcile read the release, it says so instead of reporting a revision without a record.
func TestHealFailedReleaseSuperseded(t *testing.T) {
	rel := func(v int, status release.Status, recorded bool) *release.Release {
		r := &release.Release{Name: "r", Namespace: "default", Version: v, Manifest: "kind: ConfigMap\n",
			Info: &release.Info{Status: status}, Chart: &chart.Chart{Metadata: &chart.Metadata{Name: "c", Version: "0.1.0"}}}
		markApplied(r, recorded)
		return r
	}
	for _, c := range []struct {
		name    string
		stored  []*release.Release
		wantErr string
	}{
		{"superseded", []*release.Release{rel(2, release.StatusFailed, true), rel(3, release.StatusFailed, true)}, "revision 2 is no longer the latest: revision 3"},
		{"no record", []*release.Release{rel(2, release.StatusFailed, false)}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ac := &action.Configuration{Releases: storage.Init(driver.NewMemory())}
			for _, r := range c.stored {
				if err := ac.Releases.Create(r); err != nil {
					t.Fatal(err)
				}
			}
			heal, verdicts, complete := (&client{}).healFailedRelease(ac, "r", &helmconfig.Release{Name: "r", Revision: 2}, &helmconfig.UpgradeConfig{})
			if heal.Recorded || heal.Revision != 2 || len(verdicts) != 0 || !complete {
				t.Errorf("heal = %+v, verdicts %d, complete %v", heal, len(verdicts), complete)
			}
			switch {
			case c.wantErr == "" && heal.Err != nil:
				t.Errorf("err = %v, want none: a missing record is not an error", heal.Err)
			case c.wantErr != "" && (heal.Err == nil || !strings.Contains(heal.Err.Error(), c.wantErr)):
				t.Errorf("err = %v, want %q", heal.Err, c.wantErr)
			}
		})
	}
}
