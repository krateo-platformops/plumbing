package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"

	"github.com/krateo-platformops/plumbing/valuesschema"
)

// The upstream kyverno 3.9.2 chart, vendored in the library's testdata (all dependencies resolved).
const kyvernoDir = "../../valuesschema/testdata/kyverno-3.9.2/kyverno"

func runCLI(t *testing.T, p puller, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb, p)
	return code, out.String(), errb.String()
}

func libraryInfer(t *testing.T) []byte {
	t.Helper()
	r, err := valuesschema.Infer(os.DirFS(kyvernoDir), ".")
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]byte{}, r.Schema...), '\n')
}

// packageChart writes dir as a helm chart archive, with helm's own writer, and returns its bytes.
func packageChart(t *testing.T, dir string) []byte {
	t.Helper()
	c, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	tgz, err := chartutil.Save(c, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(tgz)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// noPull fails the test if anything reaches the network boundary.
type noPull struct{ t *testing.T }

func (n noPull) Pull(context.Context, string, string) ([]byte, error) {
	n.t.Fatal("a non-OCI SOURCE reached the registry")
	return nil, nil
}

type fakePull struct {
	archive      []byte
	ref, version string
}

func (f *fakePull) Pull(_ context.Context, ref, version string) ([]byte, error) {
	f.ref, f.version = ref, version
	return f.archive, nil
}

func TestInfer_DirectoryIsTheLibrarysOutput(t *testing.T) {
	code, out, errOut := runCLI(t, noPull{t}, "infer", kyvernoDir)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != string(libraryInfer(t)) {
		t.Error("stdout is not valuesschema.Infer's schema for the same chart")
	}
	if !strings.HasPrefix(errOut, "inferred kyverno 3.9.2 (rules "+valuesschema.RulesVersion+"): ") {
		t.Errorf("summary = %q", errOut)
	}
}

// An archive and an OCI pull of the same chart give the same bytes as its directory: the three
// sources differ only in how the files arrive.
func TestInfer_ArchiveAndOCIMatchTheDirectory(t *testing.T) {
	archive := packageChart(t, kyvernoDir)
	tgz := filepath.Join(t.TempDir(), "kyverno-3.9.2.tgz")
	if err := os.WriteFile(tgz, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	want := string(libraryInfer(t))

	code, out, errOut := runCLI(t, noPull{t}, "infer", tgz)
	if code != exitOK || out != want {
		t.Errorf("archive: exit %d, schema equal %v (%s)", code, out == want, errOut)
	}

	p := &fakePull{archive: archive}
	code, out, errOut = runCLI(t, p, "infer", "oci://ghcr.io/kyverno/charts/kyverno", "--version", "3.9.2")
	if code != exitOK || out != want {
		t.Errorf("oci: exit %d, schema equal %v (%s)", code, out == want, errOut)
	}
	if p.ref != "oci://ghcr.io/kyverno/charts/kyverno" || p.version != "3.9.2" {
		t.Errorf("pulled %q %q", p.ref, p.version)
	}
}

func TestInfer_OutAndReportFiles(t *testing.T) {
	dir := t.TempDir()
	out, rep := filepath.Join(dir, "values.schema.json"), filepath.Join(dir, "report.json")
	if code, _, errOut := runCLI(t, noPull{t}, "infer", "--out", out, kyvernoDir, "--report", rep); code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if b, _ := os.ReadFile(out); string(b) != string(libraryInfer(t)) {
		t.Error("--out is not the library's schema")
	}
	var r valuesschema.Report
	b, _ := os.ReadFile(rep)
	if err := json.Unmarshal(b, &r); err != nil || r.RulesVersion != valuesschema.RulesVersion || len(r.Findings) == 0 {
		t.Errorf("report: %v, %+v", err, r)
	}
}

func TestInfer_UsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no subcommand", nil},
		{"unknown subcommand", []string{"guess", kyvernoDir}},
		{"no source", []string{"infer"}},
		{"two sources", []string{"infer", kyvernoDir, kyvernoDir}},
		{"oci without version", []string{"infer", "oci://ghcr.io/kyverno/charts/kyverno"}},
		{"version on a directory", []string{"infer", kyvernoDir, "--version", "1.0.0"}},
		{"out with wrapper", []string{"infer", kyvernoDir, "--wrapper", "w", "--chart-version", "1.0.0", "--out", "x"}},
		{"wrapper without chart-version", []string{"infer", kyvernoDir, "--wrapper", "w"}},
		{"wrapper flag without wrapper", []string{"infer", kyvernoDir, "--name", "x"}},
		{"wrapper of a directory without repository", []string{"infer", kyvernoDir, "--wrapper", filepath.Join(t.TempDir(), "w"), "--chart-version", "1.0.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := runCLI(t, noPull{t}, tc.args...); code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
		})
	}
}

func TestInfer_AChartThatCannotBeInferredExits1(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: c\nversion: 0.1.0\ndependencies:\n  - name: missing\n    version: 1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, noPull{t}, "infer", dir); code != exitFail || !strings.Contains(errOut, "helm dependency build") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

// scaffold writes a wrapper of kyverno 3.9.2 and returns its directory.
func scaffold(t *testing.T) string {
	t.Helper()
	w := filepath.Join(t.TempDir(), "kyverno")
	code, _, errOut := runCLI(t, noPull{t}, "infer", kyvernoDir, "--wrapper", w, "--chart-version", "3.9.5", "--repository", "oci://ghcr.io/kyverno/charts")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	return w
}

// #568 child 4: the generated wrapper's root has the shape of kyverno-blueprint's hand-written one at
// `global` and `kyverno` (helm/kyverno/values.schema.json at tags 3.9.3 and 3.9.4, identical at both
// keys): exactly those two properties, both objects, and `kyverno` accepting keys it does not type
// (hand-written: x-kubernetes-preserve-unknown-fields; inferred: additionalProperties: true, which
// crdgen carries as preserve-unknown). The inferred one types kyverno's values below that; the
// hand-written one cannot.
func TestWrapper_RootMatchesTheHandWrittenKyvernoBlueprint(t *testing.T) {
	w := scaffold(t)
	b, err := os.ReadFile(filepath.Join(w, "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	props, _ := doc["properties"].(map[string]any)
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "global,kyverno" {
		t.Fatalf("root properties = %v, want exactly global and kyverno", keys)
	}
	global, _ := props["global"].(map[string]any)
	if !reflect.DeepEqual(global, map[string]any{"type": "object"}) {
		t.Errorf("global = %v, want {type: object} as hand-written", global)
	}
	kyverno, _ := props["kyverno"].(map[string]any)
	if kyverno["type"] != "object" || kyverno["additionalProperties"] != true {
		t.Errorf("kyverno = type %v, additionalProperties %v; want an open object as hand-written", kyverno["type"], kyverno["additionalProperties"])
	}
	if _, typed := kyverno["properties"].(map[string]any)["admissionController"]; !typed {
		t.Error("kyverno's values are not typed below the key")
	}

	var c wrapperChart
	cb, _ := os.ReadFile(filepath.Join(w, "Chart.yaml"))
	if err := yaml.Unmarshal(cb, &c); err != nil {
		t.Fatal(err)
	}
	want := wrapperChart{APIVersion: "v2", Name: "kyverno", Type: "application", Version: "3.9.5",
		Dependencies: []wrapperDependency{{Name: "kyverno", Version: "3.9.2", Repository: "oci://ghcr.io/kyverno/charts"}}}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("Chart.yaml = %+v, want %+v", c, want)
	}
}

// vendor stands in for `helm dependency build`: it copies the upstream chart under the wrapper's
// charts/.
func vendor(t *testing.T, w string) {
	t.Helper()
	dst := filepath.Join(w, "charts", "kyverno")
	if err := os.CopyFS(dst, os.DirFS(kyvernoDir)); err != nil {
		t.Fatal(err)
	}
}

func renderClientOnly(t *testing.T, dir string) error {
	t.Helper()
	c, err := loader.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities,
		Log:          func(string, ...interface{}) {},
	}
	inst := action.NewInstall(cfg)
	inst.ClientOnly, inst.DryRun, inst.ReleaseName, inst.Namespace = true, true, "kyverno", "default"
	inst.KubeVersion = &chartutil.KubeVersion{Version: "v1.36.0", Major: "1", Minor: "36"} // kyverno needs >=1.25
	_, err = inst.RunWithContext(context.Background(), c, map[string]any{})
	return err
}

// e2e: `helm template` of the vendored wrapper, which validates the default values against the
// generated values.schema.json and renders the upstream chart's templates. CONTROL: a wrapper value
// of the wrong type for a node the schema types (crds.install is a boolean upstream) must be refused,
// so the pass above is helm validating the generated schema, not skipping it.
func TestWrapper_HelmTemplatePasses(t *testing.T) {
	w := scaffold(t)
	vendor(t, w)
	if err := renderClientOnly(t, w); err != nil {
		t.Fatalf("helm refused the generated wrapper with default values: %v", err)
	}
	if err := os.WriteFile(filepath.Join(w, "values.yaml"), []byte("kyverno:\n  crds:\n    install: \"yes\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := renderClientOnly(t, w)
	if err == nil || !strings.Contains(err.Error(), "install") {
		t.Errorf("err = %v, want a schema refusal of kyverno.crds.install", err)
	}
}

// Regenerating a vendored wrapper in place (the documented flow) gives the schema the scaffold wrote:
// one inference, whichever way the wrapper is reached.
func TestWrapper_RegenerationInPlaceMatchesTheScaffold(t *testing.T) {
	w := scaffold(t)
	vendor(t, w)
	scaffolded, err := os.ReadFile(filepath.Join(w, "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, noPull{t}, "infer", w)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != string(scaffolded) {
		t.Error("regenerating the vendored wrapper does not give the scaffolded schema")
	}
}

func TestWrapper_NeverOverwritesAChart(t *testing.T) {
	w := scaffold(t)
	before, _ := os.ReadFile(filepath.Join(w, "Chart.yaml"))
	code, _, errOut := runCLI(t, noPull{t}, "infer", kyvernoDir, "--wrapper", w, "--chart-version", "9.9.9", "--repository", "oci://x/y")
	if code != exitFail || !strings.Contains(errOut, "already holds a Chart.yaml") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if after, _ := os.ReadFile(filepath.Join(w, "Chart.yaml")); !bytes.Equal(before, after) {
		t.Error("Chart.yaml was overwritten")
	}
}

// For an OCI SOURCE the wrapper's dependency repository defaults to the reference without its chart
// name, which is what `helm dependency build` resolves.
func TestWrapper_OCIRepositoryDefault(t *testing.T) {
	w := filepath.Join(t.TempDir(), "k")
	p := &fakePull{archive: packageChart(t, kyvernoDir)}
	if code, _, errOut := runCLI(t, p, "infer", "oci://ghcr.io/kyverno/charts/kyverno", "--version", "3.9.2", "--wrapper", w, "--chart-version", "1.0.0"); code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var c wrapperChart
	cb, _ := os.ReadFile(filepath.Join(w, "Chart.yaml"))
	_ = yaml.Unmarshal(cb, &c)
	if len(c.Dependencies) != 1 || c.Dependencies[0].Repository != "oci://ghcr.io/kyverno/charts" {
		t.Errorf("dependencies = %+v", c.Dependencies)
	}
}
