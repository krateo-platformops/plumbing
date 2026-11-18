package valuesschema

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// chart is what inference reads from one chart: its metadata, its values, its own schema if it has
// one, and its dependencies (recursively).
type chart struct {
	name        string
	chartType   string // "application" or "library"
	annotations map[string]string
	deps        []dependency
	values      *value // nil when the chart has no values.yaml
	schema      []byte // the chart's own values.schema.json, nil when absent
	subcharts   map[string]*chart
}

// dependency is one entry of Chart.yaml's dependencies: the key it appears under in the parent's
// values is its alias, else its name (helm chartutil coalesceDeps).
type dependency struct {
	name  string
	alias string
}

func (d dependency) key() string {
	if d.alias != "" {
		return d.alias
	}
	return d.name
}

type chartMeta struct {
	Name         string            `yaml:"name"`
	Type         string            `yaml:"type"`
	Annotations  map[string]string `yaml:"annotations"`
	Dependencies []struct {
		Name  string `yaml:"name"`
		Alias string `yaml:"alias"`
	} `yaml:"dependencies"`
}

// loadChart reads the chart rooted at dir in fsys. Subcharts are read from dir/charts, as
// directories or as .tgz archives, keyed by their Chart.yaml name.
func loadChart(fsys fs.FS, dir string) (*chart, error) {
	read := func(name string) ([]byte, error) { return fs.ReadFile(fsys, path.Join(dir, name)) }
	return loadFrom(read, func() ([]subchartSource, error) { return listSubcharts(fsys, path.Join(dir, "charts")) })
}

type subchartSource struct {
	name string
	read func(string) ([]byte, error)
	list func() ([]subchartSource, error)
}

func loadFrom(read func(string) ([]byte, error), list func() ([]subchartSource, error)) (*chart, error) {
	raw, err := read("Chart.yaml")
	if err != nil {
		return nil, fmt.Errorf("reading Chart.yaml: %w", err)
	}
	var meta chartMeta
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("parsing Chart.yaml: %w", err)
	}
	c := &chart{name: meta.Name, chartType: meta.Type, annotations: meta.Annotations, subcharts: map[string]*chart{}}
	for _, d := range meta.Dependencies {
		c.deps = append(c.deps, dependency{name: d.Name, alias: d.Alias})
	}

	if vb, err := read("values.yaml"); err == nil {
		v, err := parseValues(vb)
		if err != nil {
			return nil, fmt.Errorf("chart %s: parsing values.yaml: %w", c.name, err)
		}
		c.values = v
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("chart %s: reading values.yaml: %w", c.name, err)
	}

	if sb, err := read("values.schema.json"); err == nil {
		c.schema = sb
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("chart %s: reading values.schema.json: %w", c.name, err)
	}

	subs, err := list()
	if err != nil {
		return nil, fmt.Errorf("chart %s: listing charts/: %w", c.name, err)
	}
	for _, s := range subs {
		sc, err := loadFrom(s.read, s.list)
		if err != nil {
			return nil, fmt.Errorf("chart %s: subchart %s: %w", c.name, s.name, err)
		}
		c.subcharts[sc.name] = sc
	}
	return c, nil
}

// listSubcharts lists dir's subcharts: each directory holding a Chart.yaml, and each .tgz. A missing
// charts/ is no subcharts; any other read failure is an error.
func listSubcharts(fsys fs.FS, dir string) ([]subchartSource, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []subchartSource
	for _, e := range entries {
		p := path.Join(dir, e.Name())
		switch {
		case e.IsDir():
			sub := p
			out = append(out, subchartSource{
				name: e.Name(),
				read: func(n string) ([]byte, error) { return fs.ReadFile(fsys, path.Join(sub, n)) },
				list: func() ([]subchartSource, error) { return listSubcharts(fsys, path.Join(sub, "charts")) },
			})
		case strings.HasSuffix(e.Name(), ".tgz"):
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				return nil, err
			}
			files, err := untar(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.Name(), err)
			}
			out = append(out, tgzSource(e.Name(), files, ""))
		}
	}
	return out, nil
}

// untar reads a chart archive into memory: path -> bytes, with the archive's single top-level
// directory kept in the path.
func untar(b []byte) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[path.Clean(h.Name)] = body
	}
}

// tgzSource is a subchart inside an in-memory archive. root is "" for the archive itself, whose
// chart is its single top-level directory.
func tgzSource(name string, files map[string][]byte, root string) subchartSource {
	if root == "" {
		for p := range files {
			if top, _, ok := strings.Cut(p, "/"); ok && strings.HasSuffix(p, top+"/Chart.yaml") && strings.Count(p, "/") == 1 {
				root = top
				break
			}
		}
	}
	return subchartSource{
		name: name,
		read: func(n string) ([]byte, error) {
			b, ok := files[path.Join(root, n)]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return b, nil
		},
		list: func() ([]subchartSource, error) {
			prefix := path.Join(root, "charts") + "/"
			seen := map[string]bool{}
			var names []string
			for p := range files {
				rest, ok := strings.CutPrefix(p, prefix)
				if !ok {
					continue
				}
				first, _, _ := strings.Cut(rest, "/")
				if !seen[first] {
					seen[first] = true
					names = append(names, first)
				}
			}
			sort.Strings(names)
			var out []subchartSource
			for _, n := range names {
				if strings.HasSuffix(n, ".tgz") {
					inner, err := untar(files[prefix+n])
					if err != nil {
						return nil, fmt.Errorf("%s: %w", n, err)
					}
					out = append(out, tgzSource(n, inner, ""))
					continue
				}
				if _, ok := files[prefix+n+"/Chart.yaml"]; ok {
					out = append(out, tgzSource(n, files, prefix+n))
				}
			}
			return out, nil
		},
	}
}
