package helm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// installClientOnly runs a REAL helm install (client-only dry run, no cluster) of a one-template
// chart, and returns the error the way plumbing's Install wraps and classifies it. The error text is
// therefore helm's own, from its engine, through its action: nothing here reconstructs the format
// under test.
func installClientOnly(t *testing.T, tmpl string, values map[string]any) error {
	t.Helper()
	ch := &chart.Chart{
		Metadata:  &chart.Metadata{APIVersion: "v2", Name: "c583", Version: "0.1.0"},
		Templates: []*chart.File{{Name: "templates/cm.yaml", Data: []byte(tmpl)}},
	}
	cfg := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...interface{}) {},
	}
	inst := action.NewInstall(cfg)
	inst.ClientOnly, inst.DryRun, inst.ReleaseName, inst.Namespace = true, true, "r583", "default"
	if _, err := inst.RunWithContext(context.Background(), ch, values); err != nil {
		return fmt.Errorf("install failed: %w", classifyTemplateExecution(err)) // exactly as Install does
	}
	return nil
}

const okTemplate = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"

// core-provider#583. A failure inside the chart's templates is classified ErrTemplateExecution,
// in all three forms helm produces, and nothing else is.
func TestClassifyTemplateExecution_AgainstRealHelm(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tmpl   string
		values map[string]any
		want   bool
	}{
		{name: "fail: helm REPLACES the error with \"execution error at (...)\"", tmpl: `{{ fail "kinds omits Repository" }}`, want: true},
		{name: "required: the same replacement", tmpl: `{{ required "a name is required" .Values.name }}`, want: true},
		{name: "a template error (nil pointer): helm keeps the typed ExecError", tmpl: `{{ .Values.a.b.c }}`, values: map[string]any{"a": nil}, want: true},
		{name: "control: a clean render", tmpl: okTemplate, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := installClientOnly(t, tc.tmpl, tc.values)
			if !tc.want {
				if err != nil {
					t.Fatalf("control render failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the render succeeded; the case does not exercise a template failure")
			}
			t.Logf("helm said: %v", err)
			if !errors.Is(err, helmconfig.ErrTemplateExecution) {
				t.Errorf("not classified ErrTemplateExecution: %v", err)
			}
		})
	}
}

// Everything else is left alone: an unrelated failure, a schema violation (its own sentinel), and an
// error already classified is not wrapped twice.
func TestClassifyTemplateExecution_LeavesOtherFailuresAlone(t *testing.T) {
	for _, err := range []error{
		errors.New("Kubernetes cluster unreachable"),
		errors.Join(helmconfig.ErrValuesSchema, errors.New("at '/name': got number, want string")),
		// The phrase QUOTED mid-message by something else (a webhook echoing a manifest annotation):
		// not helm's error, so not a template failure. Contains would have matched it.
		errors.New(`admission webhook "policy.example.com" denied the request: annotation note="execution error at (x.yaml:1:2): no" is not allowed`),
	} {
		if got := classifyTemplateExecution(err); errors.Is(got, helmconfig.ErrTemplateExecution) {
			t.Errorf("%v was classified a template failure", err)
		}
	}
	once := classifyTemplateExecution(errors.New("execution error at (c/templates/x.yaml:1:3): no"))
	if twice := classifyTemplateExecution(once); twice != once {
		t.Errorf("an already-classified error was wrapped again: %v", twice)
	}
	if classifyTemplateExecution(nil) != nil {
		t.Error("nil classified as an error")
	}
}
