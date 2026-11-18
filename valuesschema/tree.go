package valuesschema

import (
	"encoding/json"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// kind is a value's shape as values.yaml wrote it, read from the YAML node's resolved tag, so a
// quoted "1" is a string and an unquoted 1 an int.
type kind int

const (
	kindNull kind = iota
	kindBool
	kindInt
	kindFloat
	kindString
	kindMap
	kindList
)

// value is one node of values.yaml, ORDERED: a map keeps its keys in file order, which is the
// property order the portal shows.
type value struct {
	kind    kind
	scalar  any // bool, json.Number (int/float), string; nil for null
	keys    []string
	entries map[string]*value
	items   []*value
}

func parseValues(b []byte) (*value, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 { // empty file
		return &value{kind: kindMap, entries: map[string]*value{}}, nil
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, fmt.Errorf("values.yaml is not a single YAML document")
	}
	v, err := fromNode(doc.Content[0])
	if err != nil {
		return nil, err
	}
	if v.kind == kindNull {
		return &value{kind: kindMap, entries: map[string]*value{}}, nil
	}
	if v.kind != kindMap {
		return nil, fmt.Errorf("values.yaml's top level is not a map")
	}
	return v, nil
}

func fromNode(n *yaml.Node) (*value, error) {
	if n.Kind == yaml.AliasNode {
		return fromNode(n.Alias)
	}
	switch n.Kind {
	case yaml.MappingNode:
		v := &value{kind: kindMap, entries: map[string]*value{}}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if k.Tag == "!!merge" { // `<<: *anchor`: the anchored map's keys, overridable by later keys
				src := val
				if src.Kind == yaml.AliasNode {
					src = src.Alias
				}
				merged, err := fromNode(src)
				if err != nil {
					return nil, err
				}
				if merged.kind == kindMap {
					for _, mk := range merged.keys {
						v.set(mk, merged.entries[mk])
					}
				}
				continue
			}
			child, err := fromNode(val)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k.Value, err)
			}
			v.set(k.Value, child)
		}
		return v, nil
	case yaml.SequenceNode:
		v := &value{kind: kindList}
		for _, c := range n.Content {
			item, err := fromNode(c)
			if err != nil {
				return nil, err
			}
			v.items = append(v.items, item)
		}
		return v, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!null":
			return &value{kind: kindNull}, nil
		case "!!bool":
			var b bool
			if err := n.Decode(&b); err != nil {
				return nil, err
			}
			return &value{kind: kindBool, scalar: b}, nil
		case "!!int":
			var i int64
			if err := n.Decode(&i); err != nil {
				// an integer beyond int64 (or in a base Go cannot decode): keep its text exactly
				return &value{kind: kindInt, scalar: json.Number(n.Value)}, nil
			}
			return &value{kind: kindInt, scalar: json.Number(strconv.FormatInt(i, 10))}, nil
		case "!!float":
			var f float64
			if err := n.Decode(&f); err != nil {
				return nil, err
			}
			// .inf / .nan have no JSON form: untyped, no default.
			if f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308 {
				return &value{kind: kindFloat}, nil
			}
			return &value{kind: kindFloat, scalar: json.Number(strconv.FormatFloat(f, 'g', -1, 64))}, nil
		default: // !!str, !!timestamp, !!binary: helm passes these as strings
			return &value{kind: kindString, scalar: n.Value}, nil
		}
	}
	return nil, fmt.Errorf("unsupported YAML node kind %d", n.Kind)
}

// set adds or replaces key, keeping the FIRST position a key was seen at.
func (v *value) set(key string, child *value) {
	if _, ok := v.entries[key]; !ok {
		v.keys = append(v.keys, key)
	}
	v.entries[key] = child
}

// mergeOver returns base with over laid on top, as helm coalesces a parent's block for a
// dependency over the dependency's own values: maps merge key by key (over wins), anything else is
// replaced by over. Neither input is modified. Key order: base's keys first, then over's new keys.
func mergeOver(base, over *value) *value {
	if base == nil {
		return over
	}
	if over == nil {
		return base
	}
	if base.kind != kindMap || over.kind != kindMap {
		return over
	}
	out := &value{kind: kindMap, entries: map[string]*value{}}
	for _, k := range base.keys {
		out.set(k, base.entries[k])
	}
	for _, k := range over.keys {
		out.set(k, mergeOver(out.entries[k], over.entries[k]))
	}
	return out
}

// toJSON is the value as a JSON-encodable Go value, for a `default`. Map keys keep file order.
func (v *value) toJSON() any {
	switch v.kind {
	case kindMap:
		o := orderedObject{}
		for _, k := range v.keys {
			o = append(o, kv{k, v.entries[k].toJSON()})
		}
		return o
	case kindList:
		out := make([]any, 0, len(v.items))
		for _, it := range v.items {
			out = append(out, it.toJSON())
		}
		return out
	default:
		return v.scalar
	}
}
