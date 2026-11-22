package valuesschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// kv is one key of an orderedObject.
type kv struct {
	k string
	v any
}

// orderedObject is a JSON object that keeps its key order when marshalled (a Go map would sort it).
type orderedObject []kv

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(e.k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalValue(e.v)
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalValue marshals v without HTML escaping, so a default like "<none>" stays readable and the
// bytes are stable.
func marshalValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// node is one inferred schema node. Keyword order is fixed (type, int-or-string, properties, items,
// additionalProperties, default, then extra), so the output is canonical.
type node struct {
	typ         string
	intOrString bool
	properties  []kv // value: *node
	items       *node
	additional  *bool
	dflt        any
	hasDefault  bool
	raw         json.RawMessage // a subchart's own schema, emitted verbatim (D8)
	extra       []kv
}

func (n *node) object() orderedObject {
	if n.raw != nil {
		return nil
	}
	o := orderedObject{}
	if n.typ != "" {
		o = append(o, kv{"type", n.typ})
	}
	if n.intOrString {
		o = append(o, kv{"x-kubernetes-int-or-string", true})
	}
	if len(n.properties) > 0 {
		props := orderedObject{}
		for _, p := range n.properties {
			props = append(props, kv{p.k, p.v.(*node)})
		}
		o = append(o, kv{"properties", props})
	}
	if n.items != nil {
		o = append(o, kv{"items", n.items})
	}
	if n.additional != nil {
		o = append(o, kv{"additionalProperties", *n.additional})
	}
	if n.hasDefault {
		o = append(o, kv{"default", n.dflt})
	}
	return append(o, n.extra...)
}

func (n *node) MarshalJSON() ([]byte, error) {
	if n.raw != nil {
		return compactJSON(n.raw)
	}
	return n.object().MarshalJSON()
}

func (n *node) marshal() ([]byte, error) { return n.MarshalJSON() }

func compactJSON(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func boolPtr(b bool) *bool { return &b }

// inferrer walks one chart tree, recording findings.
type inferrer struct {
	report     Report
	closed     map[string]bool
	closedUsed map[string]bool
}

func (in *inferrer) find(path string, k FindingKind, detail string) {
	in.report.Findings = append(in.report.Findings, Finding{Path: path, Kind: k, Detail: detail})
}

func parseClosed(s string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".") || strings.Contains(p, "..") {
			return nil, fmt.Errorf("%s: malformed path %q", ClosedAnnotation, p)
		}
		out[p] = true
	}
	return out, nil
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// chartNode infers the schema of chart c, whose values appear at path (the root is "") and whose
// node sits at JSON pointer ptr in the output document (the root is "#").
func (in *inferrer) chartNode(c *chart, path, ptr string) (*node, error) {
	if in.closedUsed == nil {
		in.closedUsed = map[string]bool{}
	}
	values := c.values
	if values == nil {
		values = &value{kind: kindMap, entries: map[string]*value{}}
	}
	// Each dependency's key holds the dependency's own values merged UNDER the parent's block for
	// it (helm coalesceDeps), so the inferred node sees what helm will.
	deps := map[string]dependency{}
	for _, d := range c.deps {
		deps[d.key()] = d
	}
	n, err := in.mapNode(values, path, func(key string) (*node, bool, error) {
		// `global` at the top of EVERY chart's values, root or subchart: helm coalesceGlobals pushes
		// the parent's globals down and lets them win, so no chart's own global block is a schema.
		if key == "global" {
			in.find(join(path, key), KindGlobalOpen, "")
			return &node{typ: "object"}, true, nil
		}
		d, ok := deps[key]
		if !ok {
			return nil, false, nil
		}
		dn, err := in.dependencyNode(c, d, values.entries[key], join(path, key), ptr+"/properties/"+pointerEscape(key))
		return dn, true, err
	})
	if err != nil {
		return nil, err
	}
	// A dependency the parent's values.yaml does not mention still gets its key: helm always adds it.
	have := map[string]bool{}
	for _, p := range n.properties {
		have[p.k] = true
	}
	for _, d := range c.deps {
		if have[d.key()] {
			continue
		}
		dn, err := in.dependencyNode(c, d, nil, join(path, d.key()), ptr+"/properties/"+pointerEscape(d.key()))
		if err != nil {
			return nil, err
		}
		n.properties = append(n.properties, kv{d.key(), dn})
		have[d.key()] = true
	}
	// The ROOT always declares `global`, first, even when values.yaml has none: helm supplies it to
	// every release, the cdc merges its own keys into it, and a wrapper's root is expected to carry
	// it (kyverno-blueprint's hand-written schema; core-provider#568 child 4). A subchart's global is
	// only helm's push-down of the parent's, so below the root it is declared only when values.yaml
	// writes one.
	if path == "" && !have["global"] {
		in.find("global", KindGlobalOpen, "not in values.yaml; declared because helm always supplies it")
		n.properties = append([]kv{{"global", &node{typ: "object"}}}, n.properties...)
	}
	return n, nil
}

func (in *inferrer) dependencyNode(parent *chart, d dependency, parentBlock *value, path, ptr string) (*node, error) {
	sub, ok := parent.subcharts[d.name]
	if !ok {
		return nil, fmt.Errorf("dependency %s (at %s) is not vendored under charts/: run helm dependency build", d.name, path)
	}
	if sub.chartType == "library" {
		in.find(path, KindLibraryChart, sub.name)
		return &node{typ: "object"}, nil
	}
	if sub.schema != nil {
		if !json.Valid(sub.schema) {
			return nil, fmt.Errorf("subchart %s: values.schema.json is not valid JSON", sub.name)
		}
		// The subchart's schema becomes a subtree of the parent's document, so its LOCAL refs
		// ("#/$defs/x", "#/definitions/x", "#") would resolve against the parent's root and miss.
		// Rebase them onto the subtree's own pointer; crdgen indexes $defs at every nesting level by
		// full pointer (crdgen/transpile.go collectRefs), so the rebased ref resolves there.
		rebased, err := rebaseLocalRefs(sub.schema, ptr)
		if err != nil {
			return nil, fmt.Errorf("subchart %s: values.schema.json: %w", sub.name, err)
		}
		in.find(path, KindSubchartSchemaUsed, sub.name)
		// Objects whose `additionalProperties` the author left unstated are open in helm but would be
		// closed by crdgen, pruning values helm accepts (open.go). Open them BEFORE the overlay, so a
		// required map's default keeps the keys the CRD will now keep.
		tree, err := parseOrdered(rebased)
		if err != nil {
			return nil, fmt.Errorf("subchart %s: values.schema.json: %w", sub.name, err)
		}
		opened, err := marshalValue(in.openUnstated(tree, path, ptr))
		if err != nil {
			return nil, fmt.Errorf("subchart %s: values.schema.json: %w", sub.name, err)
		}
		// The coalesced values become the schema's defaults (overlay.go), so `required` is satisfied
		// as helm satisfies it.
		overlaid, err := in.overlayDefaults(opened, mergeOver(sub.values, parentBlock), path, ptr)
		if err != nil {
			return nil, fmt.Errorf("subchart %s: overlaying its values onto values.schema.json: %w", sub.name, err)
		}
		return &node{raw: json.RawMessage(overlaid)}, nil
	}
	merged := mergeOver(sub.values, parentBlock)
	view := *sub
	view.values = merged
	return in.chartNode(&view, path, ptr)
}

// mapNode infers an open object from a non-empty map. special lets the caller supply a node for a
// key (a dependency).
func (in *inferrer) mapNode(v *value, path string, special func(string) (*node, bool, error)) (*node, error) {
	n := &node{typ: "object", additional: boolPtr(true)}
	if in.closed[path] && path != "" {
		n.additional = boolPtr(false)
		in.closedUsed[path] = true
		in.find(path, KindClosedByAnnotation, "")
	}
	for _, k := range v.keys {
		child := v.entries[k]
		cp := join(path, k)
		if special != nil {
			if sn, ok, err := special(k); err != nil {
				return nil, err
			} else if ok {
				n.properties = append(n.properties, kv{k, sn})
				continue
			}
		}
		cn, err := in.valueNode(child, cp)
		if err != nil {
			return nil, err
		}
		n.properties = append(n.properties, kv{k, cn})
	}
	return n, nil
}

// valueNode is the rules table (RULES v1 in the package doc).
func (in *inferrer) valueNode(v *value, path string) (*node, error) {
	switch v.kind {
	case kindMap:
		if len(v.keys) == 0 {
			if in.closed[path] {
				return nil, fmt.Errorf("%s names %q, an empty map: there are no keys to close it to", ClosedAnnotation, path)
			}
			return &node{typ: "object"}, nil
		}
		return in.mapNode(v, path, nil)
	case kindNull:
		in.find(path, KindUntyped, "null")
		return &node{}, nil
	case kindBool:
		return &node{typ: "boolean", dflt: v.scalar, hasDefault: true}, nil
	case kindInt, kindString:
		return &node{intOrString: true, dflt: v.scalar, hasDefault: true}, nil
	case kindFloat:
		in.find(path, KindUntyped, "float")
		if v.scalar == nil {
			return &node{}, nil
		}
		return &node{dflt: v.scalar, hasDefault: true}, nil
	case kindList:
		items, err := in.itemsNode(v.items, path+"[]")
		if err != nil {
			return nil, err
		}
		return &node{typ: "array", items: items, dflt: v.toJSON(), hasDefault: true}, nil
	}
	return nil, fmt.Errorf("%s: unknown value kind", path)
}

// itemsNode infers a list's items: nil for an empty list, the shared type when every element has
// one kind (int and string count as one, int-or-string), a merge of every element's keys for a list
// of maps, and untyped otherwise.
//
// An empty list emits no `items`, so its items accept any value (crdgen makes them
// preserve-unknown) and are reported KindUntyped like every other any-value path. Only the report
// says so: the schema is the same `{type: array, default: []}` it always was.
func (in *inferrer) itemsNode(items []*value, path string) (*node, error) {
	if len(items) == 0 {
		in.find(path, KindUntyped, "empty list")
		return nil, nil
	}
	class := func(k kind) kind {
		if k == kindInt {
			return kindString // one class: int-or-string
		}
		return k
	}
	first := class(items[0].kind)
	for _, it := range items[1:] {
		if class(it.kind) != first {
			in.find(path, KindUntyped, "mixed list")
			return &node{}, nil
		}
	}
	switch first {
	case kindMap:
		merged := &value{kind: kindMap, entries: map[string]*value{}}
		for _, it := range items {
			merged = mergeOver(merged, it)
		}
		n, err := in.valueNode(merged, path)
		if err != nil {
			return nil, err
		}
		n.hasDefault, n.dflt = false, nil // the list carries the default, not its element type
		stripDefaults(n)
		return n, nil
	case kindList:
		in.find(path, KindUntyped, "list of lists")
		return &node{}, nil
	case kindNull, kindFloat:
		in.find(path, KindUntyped, map[kind]string{kindNull: "list of nulls", kindFloat: "list of floats"}[first])
		return &node{}, nil
	case kindBool:
		return &node{typ: "boolean"}, nil
	default:
		return &node{intOrString: true}, nil
	}
}

// stripDefaults removes every default below an item schema: the values of ONE element are not a
// default for every element (the list's own default carries them).
func stripDefaults(n *node) {
	if n == nil {
		return
	}
	n.hasDefault, n.dflt = false, nil
	for _, p := range n.properties {
		stripDefaults(p.v.(*node))
	}
	stripDefaults(n.items)
}
