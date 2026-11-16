package helm

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/action"
)

// Regression guard: applyUpgradeConfig used to omit TakeOwnership entirely, so a caller's opt-in to
// adopt pre-existing, un-owned resources was silently dropped on every Upgrade — Helm's ownership
// gate (existingResourceConflict -> "invalid ownership metadata") fired despite the opt-in. cdc sets
// UpgradeConfig.TakeOwnership=true on all its upgrade paths precisely to self-heal; it must reach the
// action.
func TestApplyUpgradeConfig_PropagatesTakeOwnership(t *testing.T) {
	up := &action.Upgrade{}
	applyUpgradeConfig(up, "ns", &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{TakeOwnership: true}})
	if !up.TakeOwnership {
		t.Fatal("applyUpgradeConfig dropped TakeOwnership=true — Helm's ownership gate will fire despite the opt-in")
	}

	off := &action.Upgrade{}
	applyUpgradeConfig(off, "ns", &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{TakeOwnership: false}})
	if off.TakeOwnership {
		t.Fatal("applyUpgradeConfig set TakeOwnership when the caller did not request it")
	}
}

// The Install path already honored it; keep both covered so the two apply* functions can't drift.
func TestApplyInstallConfig_PropagatesTakeOwnership(t *testing.T) {
	in := &action.Install{}
	applyInstallConfig(in, "rel", "ns", &helmconfig.InstallConfig{ActionConfig: &helmconfig.ActionConfig{TakeOwnership: true}})
	if !in.TakeOwnership {
		t.Fatal("applyInstallConfig dropped TakeOwnership=true")
	}
}

// plumbing#72, and #115 before it: an ActionConfig field that nothing reads is a knob that silently
// does nothing (NoHooks, SkipCRDs, SkipSchemaValidation and Verify were four of them). Every field
// must reach helm's action.Install and action.Upgrade, unless it is listed below with where else it
// is consumed. A new field fails here until it is wired or listed.
func TestEveryActionConfigFieldReachesTheHelmAction(t *testing.T) {
	consumedElsewhere := map[string]string{
		"ChartName": "the chart getter's repo (buildGetterOpts)",
		"Values":    "passed to RunWithContext, and to validateValuesSchema",
		"Verbose":   "the action config's kube client logging (newActionConfig)",
		"LogWriter": "the action config's kube client logging (newActionConfig)",
		"Verify":    "plumbing#72's second half: provenance is checked in plumbing's own chart getter, which helm's ChartPathOptions.Verify never reaches",
	}
	installOnly := map[string]string{
		"Replace":     "action.Upgrade has no Replace",
		"IncludeCRDs": "action.Upgrade has no IncludeCRDs",
	}
	samples := map[string]any{
		"Timeout":      time.Minute,
		"DryRun":       helmconfig.DryRunServer,
		"PostRenderer": nopPostRenderer{},
		"Labels":       map[string]string{"k": "v"},
		"Values":       map[string]any{"k": "v"},
	}
	typ := reflect.TypeOf(helmconfig.ActionConfig{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if _, ok := consumedElsewhere[f.Name]; ok {
			continue
		}
		cfg := helmconfig.ActionConfig{}
		v := reflect.ValueOf(&cfg).Elem().Field(i)
		switch {
		case samples[f.Name] != nil:
			v.Set(reflect.ValueOf(samples[f.Name]))
		case v.Kind() == reflect.Bool:
			v.SetBool(true)
		case v.Kind() == reflect.String:
			v.SetString("x")
		default:
			t.Fatalf("ActionConfig.%s (%s) has no sample value: add one so the field is checked", f.Name, f.Type)
		}

		in, inBase := &action.Install{}, &action.Install{}
		applyInstallConfig(in, "r", "ns", &helmconfig.InstallConfig{ActionConfig: &cfg})
		applyInstallConfig(inBase, "r", "ns", &helmconfig.InstallConfig{ActionConfig: &helmconfig.ActionConfig{}})
		if reflect.DeepEqual(in, inBase) {
			t.Errorf("ActionConfig.%s set changes nothing on action.Install: it is silently ignored", f.Name)
		}
		if _, ok := installOnly[f.Name]; ok {
			continue
		}
		up, upBase := &action.Upgrade{}, &action.Upgrade{}
		applyUpgradeConfig(up, "ns", &helmconfig.UpgradeConfig{ActionConfig: &cfg})
		applyUpgradeConfig(upBase, "ns", &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{}})
		if reflect.DeepEqual(up, upBase) {
			t.Errorf("ActionConfig.%s set changes nothing on action.Upgrade: it is silently ignored", f.Name)
		}
	}
}

type nopPostRenderer struct{}

func (nopPostRenderer) Run(b *bytes.Buffer) (*bytes.Buffer, error) { return b, nil }

// The three skips, each way, on both actions.
func TestApplyConfig_PropagatesTheSkipSwitches(t *testing.T) {
	for _, on := range []bool{true, false} {
		cfg := &helmconfig.ActionConfig{NoHooks: on, SkipCRDs: on, SkipSchemaValidation: on}
		in := &action.Install{}
		applyInstallConfig(in, "r", "ns", &helmconfig.InstallConfig{ActionConfig: cfg})
		if in.DisableHooks != on || in.SkipCRDs != on || in.SkipSchemaValidation != on {
			t.Errorf("install, switches %v: DisableHooks=%v SkipCRDs=%v SkipSchemaValidation=%v", on, in.DisableHooks, in.SkipCRDs, in.SkipSchemaValidation)
		}
		up := &action.Upgrade{}
		applyUpgradeConfig(up, "ns", &helmconfig.UpgradeConfig{ActionConfig: cfg})
		if up.DisableHooks != on || up.SkipCRDs != on || up.SkipSchemaValidation != on {
			t.Errorf("upgrade, switches %v: DisableHooks=%v SkipCRDs=%v SkipSchemaValidation=%v", on, up.DisableHooks, up.SkipCRDs, up.SkipSchemaValidation)
		}
	}
}
