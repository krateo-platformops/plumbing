package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing/fstest"

	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/registry"

	"github.com/krateo-platformops/plumbing/valuesschema"
)

const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// puller fetches a chart archive from an OCI registry. It is the one network boundary, so tests
// substitute it; production uses helm's own registry client.
type puller interface {
	Pull(ctx context.Context, ref, version string) ([]byte, error)
}

type helmPuller struct{}

// Pull uses helm's registry client, with helm's credentials file (helm registry login), exactly as
// `helm pull oci://...` does. OCI tags cannot carry "+", so helm stores a semver build suffix as "_".
func (helmPuller) Pull(_ context.Context, ref, version string) ([]byte, error) {
	c, err := registry.NewClient(registry.ClientOptWriter(io.Discard))
	if err != nil {
		return nil, err
	}
	res, err := c.Pull(strings.TrimPrefix(ref, "oci://")+":"+strings.ReplaceAll(version, "+", "_"), registry.PullOptWithChart(true))
	if err != nil {
		return nil, err
	}
	if res.Chart == nil || len(res.Chart.Data) == 0 {
		return nil, fmt.Errorf("%s:%s: the registry returned no chart layer", ref, version)
	}
	return res.Chart.Data, nil
}

type options struct {
	source       string
	version      string
	out          string
	report       string
	wrapper      string
	chartVersion string
	name         string
	repository   string
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, p puller) int {
	if len(args) == 0 || args[0] != "infer" {
		fmt.Fprintln(stderr, "usage: values-schema infer SOURCE [--version V] [--out FILE | --wrapper DIR --chart-version V [--name N] [--repository R]] [--report FILE]")
		return exitUsage
	}
	o, err := parseInfer(args[1:], stderr)
	if err != nil {
		fmt.Fprintln(stderr, "values-schema:", err)
		return exitUsage
	}
	if err := infer(ctx, o, stdout, stderr, p); err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(stderr, "values-schema:", err)
			return exitUsage
		}
		fmt.Fprintln(stderr, "values-schema:", err)
		return exitFail
	}
	return exitOK
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// parseInfer parses infer's flags and its one positional SOURCE, which may come before, between or
// after the flags (the standard flag package stops at the first positional argument).
func parseInfer(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("infer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.version, "version", "", "chart version (required for an oci:// SOURCE)")
	fs.StringVar(&o.out, "out", "", "write the schema here instead of stdout")
	fs.StringVar(&o.report, "report", "", "write the inference report (JSON) here")
	fs.StringVar(&o.wrapper, "wrapper", "", "scaffold a wrapper chart depending on SOURCE in this directory")
	fs.StringVar(&o.chartVersion, "chart-version", "", "the wrapper chart's own version (with --wrapper)")
	fs.StringVar(&o.name, "name", "", "the wrapper chart's name (with --wrapper; default: SOURCE's chart name)")
	fs.StringVar(&o.repository, "repository", "", "the dependency's repository in the wrapper's Chart.yaml (with --wrapper; default for oci://: the reference without its chart name)")
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return o, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return o, fmt.Errorf("infer takes exactly one SOURCE, got %d", len(positional))
	}
	o.source = positional[0]
	if o.wrapper != "" && o.out != "" {
		return o, errors.New("--out and --wrapper are exclusive: a wrapper's schema is written into the wrapper")
	}
	if o.wrapper == "" && (o.chartVersion != "" || o.name != "" || o.repository != "") {
		return o, errors.New("--chart-version, --name and --repository only apply with --wrapper")
	}
	if o.wrapper != "" && o.chartVersion == "" {
		return o, errors.New("--wrapper needs --chart-version, the wrapper chart's own version")
	}
	isOCI := strings.HasPrefix(o.source, "oci://")
	if isOCI && o.version == "" {
		return o, errors.New("an oci:// SOURCE needs --version")
	}
	if !isOCI && o.version != "" {
		return o, errors.New("--version applies to an oci:// SOURCE only; a directory or archive is already one version")
	}
	return o, nil
}

// source is a loaded chart: an fs.FS and the chart's directory inside it, with its Chart.yaml name
// and version.
type source struct {
	fsys    fs.FS
	dir     string
	name    string
	version string
}

func load(ctx context.Context, o options, p puller) (*source, error) {
	var archive []byte
	switch {
	case strings.HasPrefix(o.source, "oci://"):
		b, err := p.Pull(ctx, o.source, o.version)
		if err != nil {
			return nil, fmt.Errorf("pulling %s %s: %w", o.source, o.version, err)
		}
		archive = b
	default:
		st, err := os.Stat(o.source)
		if err != nil {
			return nil, err
		}
		if st.IsDir() {
			return withMeta(&source{fsys: os.DirFS(o.source), dir: "."})
		}
		b, err := os.ReadFile(o.source)
		if err != nil {
			return nil, err
		}
		archive = b
	}
	// helm's own archive reader: it strips the archive's top directory and refuses unsafe paths.
	files, err := loader.LoadArchiveFiles(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("reading the chart archive: %w", err)
	}
	m := fstest.MapFS{}
	for _, f := range files {
		m["chart/"+f.Name] = &fstest.MapFile{Data: f.Data}
	}
	return withMeta(&source{fsys: m, dir: "chart"})
}

func withMeta(s *source) (*source, error) {
	raw, err := fs.ReadFile(s.fsys, path.Join(s.dir, "Chart.yaml"))
	if err != nil {
		return nil, fmt.Errorf("reading Chart.yaml: %w", err)
	}
	var meta struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("parsing Chart.yaml: %w", err)
	}
	if meta.Name == "" || meta.Version == "" {
		return nil, errors.New("Chart.yaml has no name or no version")
	}
	s.name, s.version = meta.Name, meta.Version
	return s, nil
}

func infer(ctx context.Context, o options, stdout, stderr io.Writer, p puller) error {
	src, err := load(ctx, o, p)
	if err != nil {
		return err
	}
	if o.wrapper != "" {
		return scaffoldWrapper(o, src, stderr)
	}
	r, err := valuesschema.Infer(src.fsys, src.dir)
	if err != nil {
		return err
	}
	if err := writeOutputs(o.out, o.report, r, stdout); err != nil {
		return err
	}
	summarize(stderr, src.name+" "+src.version, r.Report)
	return nil
}

func writeOutputs(out, report string, r *valuesschema.Result, stdout io.Writer) error {
	schema := append(append([]byte{}, r.Schema...), '\n')
	if out == "" {
		if _, err := stdout.Write(schema); err != nil {
			return err
		}
	} else if err := os.WriteFile(out, schema, 0o644); err != nil {
		return err
	}
	if report == "" {
		return nil
	}
	b, err := json.MarshalIndent(r.Report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(report, append(b, '\n'), 0o644)
}

// summarize prints one line: what was inferred, under which rules, and how many findings of each
// kind, sorted by kind, so a reviewer sees at a glance what to look at in the report.
func summarize(w io.Writer, what string, rep valuesschema.Report) {
	counts := map[valuesschema.FindingKind]int{}
	for _, f := range rep.Findings {
		counts[f.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[valuesschema.FindingKind(k)]))
	}
	fmt.Fprintf(w, "inferred %s (rules %s): %d finding(s) %s\n", what, rep.RulesVersion, len(rep.Findings), strings.Join(parts, " "))
}

// wrapperChart is the wrapper's Chart.yaml, in the field order a reviewer expects.
type wrapperChart struct {
	APIVersion   string              `yaml:"apiVersion"`
	Name         string              `yaml:"name"`
	Type         string              `yaml:"type"`
	Version      string              `yaml:"version"`
	Dependencies []wrapperDependency `yaml:"dependencies"`
}

type wrapperDependency struct {
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	Repository string `yaml:"repository"`
}

const wrapperValues = `# Values for the wrapped chart go under its key, as for any helm dependency. After changing this
# file, regenerate values.schema.json (helm dependency build, then values-schema infer on this
# directory): the parent's block is merged over the dependency's own values, so the schema's
# defaults follow what is set here.
`

// scaffoldWrapper writes a new wrapper chart into o.wrapper: Chart.yaml, values.yaml and the
// values.schema.json inferred for the wrapper itself. It never overwrites an existing chart.
func scaffoldWrapper(o options, src *source, stderr io.Writer) error {
	repo := o.repository
	if repo == "" && strings.HasPrefix(o.source, "oci://") {
		repo = o.source[:strings.LastIndex(o.source, "/")]
	}
	if repo == "" {
		return usageError{"--wrapper needs --repository for a directory or archive SOURCE: the wrapper's Chart.yaml must say where helm fetches the dependency"}
	}
	name := o.name
	if name == "" {
		name = src.name
	}
	if _, err := os.Stat(filepath.Join(o.wrapper, "Chart.yaml")); err == nil {
		return fmt.Errorf("%s already holds a Chart.yaml: to regenerate an existing wrapper's schema, run `values-schema infer %s --out %s`", o.wrapper, o.wrapper, filepath.Join(o.wrapper, "values.schema.json"))
	}
	chartYAML, err := yaml.Marshal(wrapperChart{
		APIVersion: "v2", Name: name, Type: "application", Version: o.chartVersion,
		Dependencies: []wrapperDependency{{Name: src.name, Version: src.version, Repository: repo}},
	})
	if err != nil {
		return err
	}

	// Infer the wrapper as helm will see it once `helm dependency build` has vendored SOURCE: the
	// wrapper's own (empty) values, and SOURCE under charts/.
	w := fstest.MapFS{
		"w/Chart.yaml":  {Data: chartYAML},
		"w/values.yaml": {Data: []byte(wrapperValues)},
	}
	if err := fs.WalkDir(src.fsys, src.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(src.fsys, p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, src.dir), "/")
		if src.dir == "." {
			rel = p
		}
		w["w/charts/"+src.name+"/"+rel] = &fstest.MapFile{Data: b}
		return nil
	}); err != nil {
		return err
	}
	r, err := valuesschema.Infer(w, "w")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(o.wrapper, 0o755); err != nil {
		return err
	}
	for name, data := range map[string][]byte{"Chart.yaml": chartYAML, "values.yaml": []byte(wrapperValues)} {
		if err := os.WriteFile(filepath.Join(o.wrapper, name), data, 0o644); err != nil {
			return err
		}
	}
	if err := writeOutputs(filepath.Join(o.wrapper, "values.schema.json"), o.report, r, io.Discard); err != nil {
		return err
	}
	summarize(stderr, "wrapper "+name+" "+o.chartVersion+" of "+src.name+" "+src.version, r.Report)
	return nil
}
