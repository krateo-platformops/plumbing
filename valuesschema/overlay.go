package valuesschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Overlaying a subchart's coalesced values onto its own schema (authn's review of plumbing#215, (2);
// Diego, 2026-11-18: "overlay the defaults").
//
// Helm validates a subchart's schema against its COALESCED values: its values.yaml with the parent's
// block merged over (helm v3.20.2 chartutil values.go:161-167, then jsonschema.go recursing into each
// dependency). So `required: [foo]` with `foo: bar` in values.yaml is satisfied in helm even when the
// user omits foo. The CRD validates only what the user wrote, so the verbatim schema refused that
// composition. Putting each coalesced leaf on its schema node as `default` lets the apiserver fill it
// before `required` is checked, as helm's coalesce does. Rules:
//
//   - A leaf is a scalar or a list (the whole list is the default, as helm replaces lists). A map
//     recurses into the node's `properties`.
//   - The coalesced value REPLACES an authored `default`: helm never applies schema defaults, the
//     values are what a release gets.
//   - A node behind a `$ref` is INLINED at that use site before its leaves are defaulted. crdgen drops
//     a `default` written beside a `$ref` (transpile.go: only description and the preserve marker
//     survive), and defaulting the shared `$def` would apply one use site's value to every other.
//     crdgen inlines every `$ref` anyway (structural schemas carry none), so this only does it
//     earlier, and only where a default is needed.
//   - Nothing is invented: a value key with no schema node, a map whose node has no `properties`
//     (an open map), a `null`, and an unresolvable `$ref` are skipped and reported.
//   - A coalesced value that contradicts its authored type is still overlaid. That chart fails its
//     own schema in helm too, and crdgen's defaultcheck reports it downstream; hiding it here would
//     turn a broken chart into a silently different one.

// parseOrdered decodes JSON into the package's ordered form: an object is an orderedObject (key order
// kept), an array is []any, numbers stay json.Number.
func parseOrdered(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := readOrdered(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after the schema")
	}
	return v, nil
}

func readOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		o := orderedObject{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, ok := kt.(string)
			if !ok {
				return nil, fmt.Errorf("object key %v is not a string", kt)
			}
			v, err := readOrdered(dec)
			if err != nil {
				return nil, err
			}
			o = append(o, kv{k, v})
		}
		_, err := dec.Token()
		return o, err
	case '[':
		a := []any{}
		for dec.More() {
			v, err := readOrdered(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := dec.Token()
		return a, err
	}
	return nil, fmt.Errorf("unexpected delimiter %v", d)
}

func (o orderedObject) get(k string) (any, bool) {
	for _, e := range o {
		if e.k == k {
			return e.v, true
		}
	}
	return nil, false
}

// set replaces k's value in place, or appends k.
func (o orderedObject) set(k string, v any) orderedObject {
	for i := range o {
		if o[i].k == k {
			o[i].v = v
			return o
		}
	}
	return append(o, kv{k, v})
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case orderedObject:
		out := make(orderedObject, len(t))
		for i, e := range t {
			out[i] = kv{e.k, deepCopy(e.v)}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}

// resolvePointer finds the node at ref ("#/a/b", already rebased onto the parent document) inside the
// embedded subtree, whose own pointer is base.
func resolvePointer(subtree any, base, ref string) (any, bool) {
	if ref == base {
		return subtree, true
	}
	rest, ok := strings.CutPrefix(ref, base+"/")
	if !ok {
		return nil, false // outside this subtree: not ours to resolve
	}
	cur := subtree
	for _, tok := range strings.Split(rest, "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		o, ok := cur.(orderedObject)
		if !ok {
			return nil, false
		}
		if cur, ok = o.get(tok); !ok {
			return nil, false
		}
	}
	return cur, true
}

// overlayer overlays one embedded subchart schema.
type overlayer struct {
	in      *inferrer
	subtree any    // the rebased subchart schema
	base    string // its JSON pointer in the parent document
}

// derefInline returns n with any `$ref` chain inlined: a copy of the target, with n's own siblings
// (other than `$ref`) laid over it. ok is false when a ref cannot be resolved inside the subtree.
func (ov *overlayer) derefInline(n orderedObject, path string) (orderedObject, bool) {
	for hops := 0; hops < 32; hops++ {
		rv, has := n.get("$ref")
		if !has {
			return n, true
		}
		ref, _ := rv.(string)
		target, ok := resolvePointer(ov.subtree, ov.base, ref)
		to, isObj := target.(orderedObject)
		if !ok || !isObj {
			ov.in.find(path, KindOverlaySkipped, "unresolvable $ref "+ref)
			return n, false
		}
		inlined := deepCopy(to).(orderedObject)
		for _, e := range n {
			if e.k != "$ref" {
				inlined = inlined.set(e.k, deepCopy(e.v))
			}
		}
		ov.in.find(path, KindRefInlined, ref)
		n = inlined
	}
	ov.in.find(path, KindOverlaySkipped, "$ref chain longer than 32")
	return n, false
}

// overlay lays the coalesced value v onto schema node n (at path) and returns the updated node.
func (ov *overlayer) overlay(n orderedObject, v *value, path string) orderedObject {
	for _, k := range v.keys {
		cv := v.entries[k]
		cp := join(path, k)
		if cv.kind == kindNull {
			ov.in.find(cp, KindOverlaySkipped, "null in values")
			continue
		}
		pv, _ := n.get("properties")
		props, ok := pv.(orderedObject)
		if !ok {
			ov.in.find(cp, KindOverlaySkipped, "the schema declares no properties here (an open map)")
			continue
		}
		sv, ok := props.get(k)
		sn, isObj := sv.(orderedObject)
		if !ok || !isObj {
			ov.in.find(cp, KindOverlaySkipped, "no schema node for this key")
			continue
		}
		sn, ok = ov.derefInline(sn, cp)
		if !ok {
			continue
		}
		if cv.kind == kindMap && len(cv.keys) > 0 {
			sn = ov.overlay(sn, cv, cp)
		} else {
			_, had := sn.get("default")
			sn = sn.set("default", cv.toJSON())
			detail := ""
			if had {
				detail = "replaced the authored default"
			}
			ov.in.find(cp, KindDefaultOverlaid, detail)
		}
		props = props.set(k, sn)
		n = n.set("properties", props)
	}
	return n
}

// overlayDefaults returns the rebased subchart schema with the coalesced values overlaid.
func (in *inferrer) overlayDefaults(rebased []byte, values *value, path, ptr string) ([]byte, error) {
	if values == nil || len(values.keys) == 0 {
		return rebased, nil
	}
	tree, err := parseOrdered(rebased)
	if err != nil {
		return nil, err
	}
	root, ok := tree.(orderedObject)
	if !ok {
		in.find(path, KindOverlaySkipped, "the subchart schema is not an object")
		return rebased, nil
	}
	ov := &overlayer{in: in, subtree: root, base: ptr}
	out := ov.overlay(root, values, path)
	return marshalValue(out)
}
