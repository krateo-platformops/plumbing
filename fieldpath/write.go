package fieldpath

import (
	"errors"
	"fmt"
)

// # The writer
//
// Set and Remove are Get's writers, in Get's dialect (plumbing#111; decided by Diego 2026-11-10:
// "a strict writer"). A path means the same thing to all three: a segment is dispatched on its Kind,
// never on how its text looks, and anything Get refuses to read through, the writers refuse to write
// through.
//
// # The rules
//
//	key on a map          the key; an absent key creates the next container (below)
//	index on an array     < len replaces, == len APPENDS, > len is REFUSED: no nil holes. Get reads
//	                      past the end as absent, and a writer that padded would decide what the gap
//	                      holds, which no caller asked for.
//	[?k=v] on an array    must match EXACTLY ONE existing element. No match is refused (a write never
//	                      invents an element), several are refused as in Get.
//	contradicting Kind    refused, as in Get: a key on an array, an index or selector on a map.
//	scalar or null where  refused, never overwritten. Get calls a null in the middle of a path a shape
//	  a container must be error, and overwriting a value to make room for a path is the destructive
//	                      default a strict writer exists to avoid.
//
// An ABSENT container is created by the Kind of the segment that addresses it (oasgen-provider's
// refinement on #111): a key creates a map, an index creates an array (into which only index 0, == len,
// can then be written), and a selector creates nothing, because it would have to invent the element it
// selects. Request bodies are usually built from nothing: `credentials[0].value` on an empty body
// creates {"credentials": [{"value": ...}]}.
//
// # A bare numeric segment is a key
//
// Parse types `credentials.0.value` as the KEY "0" (segments are typed by how they were written), and
// the writer does not second-guess it. Against an existing array that path is refused, exactly as Get
// refuses to read it (oasgen-provider#544 refuses it for reads too). On an EMPTY body there is no array
// to refuse against, so it creates a map: {"credentials":{"0":{"value":...}}}. That is the one place the
// dialect builds a structure the author may not have meant instead of refusing; write `credentials[0]`
// to create an array. Guessing an array from the digits would make Set disagree with Get, invisibly,
// until a read failed on data the write had built.
//
// # Atomic
//
// A refused write changes nothing. New containers are attached to their parent only after everything
// beneath them succeeded, and the only in-place mutation, the final assignment, happens after the
// whole path has been checked, so a path refused at its last segment leaves no empty maps behind.
//
// # The root
//
// Both return the root, because a write may have to create it (obj == nil) or replace it (an append to
// a root array). Use the returned value, not obj.

// ErrEmptyPath is returned for a write or removal with no segments: it would replace or delete the
// whole document, which no field mapping means.
var ErrEmptyPath = errors.New("fieldpath: a write needs at least one segment")

// Set writes value at segs in obj and returns the root. obj may be nil, in which case the root is
// created by the first segment's Kind. On error the document is unchanged.
func Set(obj any, segs []Segment, value any) (any, error) {
	if len(segs) == 0 {
		return obj, ErrEmptyPath
	}
	root := obj
	if root == nil {
		c, err := newContainer(segs, 0)
		if err != nil {
			return obj, err
		}
		root = c
	}
	out, err := setAt(root, segs, 0, value)
	if err != nil {
		return obj, err
	}
	return out, nil
}

// setAt writes value at segs[i:] beneath cur and returns cur, updated. It attaches nothing to cur until
// the write beneath has succeeded.
func setAt(cur any, segs []Segment, i int, value any) (any, error) {
	seg := segs[i]
	last := i == len(segs)-1
	switch c := cur.(type) {
	case map[string]any:
		switch seg.Kind {
		case KindKey:
			if last {
				c[seg.Key] = value
				return c, nil
			}
			child, ok := c[seg.Key]
			if !ok {
				nc, err := newContainer(segs, i+1)
				if err != nil {
					return nil, err
				}
				child = nc
			}
			updated, err := setAt(child, segs, i+1, value)
			if err != nil {
				return nil, err
			}
			c[seg.Key] = updated
			return c, nil
		case KindIndex:
			return nil, fmt.Errorf("segment %d (%s): an array index cannot address a map; quote it as ['%d'] if it is a key", i, seg.String(), seg.Index)
		case KindPredicate:
			return nil, fmt.Errorf("segment %d (%s): a [?key=value] selector cannot address a map; it selects an element of an array", i, seg.String())
		default:
			return nil, fmt.Errorf("segment %d: unknown segment kind %v", i, seg.Kind)
		}
	case []any:
		var idx int
		switch seg.Kind {
		case KindIndex:
			idx = seg.Index
			if idx < 0 || idx > len(c) {
				return nil, fmt.Errorf("segment %d (%s): index %d is past the end of an array of %d; a write appends at index %d and never pads", i, seg.String(), idx, len(c), len(c))
			}
		case KindPredicate:
			hit, found, err := matchPredicate(c, seg.Key, seg.Value)
			if err != nil {
				return nil, fmt.Errorf("segment %d (%s): %w", i, seg.String(), err)
			}
			if !found {
				return nil, fmt.Errorf("segment %d (%s): the selector matches no element, and a write never invents one", i, seg.String())
			}
			idx = hit
		case KindKey:
			return nil, fmt.Errorf("segment %d (%s): a map key cannot address an array element; use [%s] without quotes if you meant an index", i, seg.String(), seg.Key)
		default:
			return nil, fmt.Errorf("segment %d: unknown segment kind %v", i, seg.Kind)
		}
		appending := idx == len(c)
		var child any
		if last {
			child = value
		} else {
			if appending {
				nc, err := newContainer(segs, i+1)
				if err != nil {
					return nil, err
				}
				child = nc
			} else {
				child = c[idx]
			}
			updated, err := setAt(child, segs, i+1, value)
			if err != nil {
				return nil, err
			}
			child = updated
		}
		if appending {
			return append(c, child), nil
		}
		c[idx] = child
		return c, nil
	default:
		return nil, fmt.Errorf("segment %d (%s): %T is neither a map nor an array; a write does not overwrite it to make room", i, seg.String(), cur)
	}
}

// newContainer creates the absent container that segs[i] addresses, by segs[i]'s Kind.
func newContainer(segs []Segment, i int) (any, error) {
	seg := segs[i]
	switch seg.Kind {
	case KindKey:
		return map[string]any{}, nil
	case KindIndex:
		return []any{}, nil
	case KindPredicate:
		return nil, fmt.Errorf("segment %d (%s): the array it selects from does not exist, and a write never invents the element a selector names", i, seg.String())
	default:
		return nil, fmt.Errorf("segment %d: unknown segment kind %v", i, seg.Kind)
	}
}

// Remove deletes the value at segs in obj and returns the root and whether there was one.
//
// The same three-way contract as Get: found=false with a nil error means the path does not resolve
// against this data, so there was nothing to remove (a no-op); an error means the path cannot APPLY
// to it (a contradicting Kind, an ambiguous selector, a scalar or null mid-path), and the document is
// unchanged. Removing an array element shifts the ones after it.
func Remove(obj any, segs []Segment) (any, bool, error) {
	if len(segs) == 0 {
		return obj, false, ErrEmptyPath
	}
	if obj == nil {
		return obj, false, nil
	}
	out, found, err := removeAt(obj, segs, 0)
	if err != nil || !found {
		return obj, false, err
	}
	return out, true, nil
}

func removeAt(cur any, segs []Segment, i int) (any, bool, error) {
	seg := segs[i]
	last := i == len(segs)-1
	switch c := cur.(type) {
	case map[string]any:
		switch seg.Kind {
		case KindKey:
			child, ok := c[seg.Key]
			if !ok {
				return c, false, nil
			}
			if last {
				delete(c, seg.Key)
				return c, true, nil
			}
			updated, found, err := removeAt(child, segs, i+1)
			if err != nil || !found {
				return c, false, err
			}
			c[seg.Key] = updated
			return c, true, nil
		case KindIndex:
			return nil, false, fmt.Errorf("segment %d (%s): an array index cannot address a map; quote it as ['%d'] if it is a key", i, seg.String(), seg.Index)
		case KindPredicate:
			return nil, false, fmt.Errorf("segment %d (%s): a [?key=value] selector cannot address a map; it selects an element of an array", i, seg.String())
		default:
			return nil, false, fmt.Errorf("segment %d: unknown segment kind %v", i, seg.Kind)
		}
	case []any:
		var idx int
		switch seg.Kind {
		case KindIndex:
			if seg.Index < 0 || seg.Index >= len(c) {
				return c, false, nil // absent, as Get reads it
			}
			idx = seg.Index
		case KindPredicate:
			hit, found, err := matchPredicate(c, seg.Key, seg.Value)
			if err != nil {
				return nil, false, fmt.Errorf("segment %d (%s): %w", i, seg.String(), err)
			}
			if !found {
				return c, false, nil
			}
			idx = hit
		case KindKey:
			return nil, false, fmt.Errorf("segment %d (%s): a map key cannot address an array element; use [%s] without quotes if you meant an index", i, seg.String(), seg.Key)
		default:
			return nil, false, fmt.Errorf("segment %d: unknown segment kind %v", i, seg.Kind)
		}
		if last {
			out := make([]any, 0, len(c)-1)
			out = append(out, c[:idx]...)
			return append(out, c[idx+1:]...), true, nil
		}
		updated, found, err := removeAt(c[idx], segs, i+1)
		if err != nil || !found {
			return c, false, err
		}
		c[idx] = updated
		return c, true, nil
	default:
		return nil, false, fmt.Errorf("segment %d (%s): %T is neither a map nor an array", i, seg.String(), cur)
	}
}
