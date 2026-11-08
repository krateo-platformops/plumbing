package fieldpath

import (
	"fmt"
)

// # The walker
//
// Get is the WALKER beside this package's parser. Ported from oasgen-provider
// (feat/fieldpath-walker-for-plumbing, a2a8176), where it was staged beside rest-dynamic-controller's
// GetNestedField and checked against it by a differential test that stays there.
//
// # Why the walker belongs with the parser
//
// The parser decides what a path MEANS; the walker decides what it SELECTS. For a predicate those are not
// separable -- the uniqueness rule that makes `[?type=otp]` safe lives in the walker, not the grammar. A
// grammar shared and a selection rule copied is not a shared dialect; it is the divergence with an extra
// step.
//
// The concrete case: oasgen-provider's #510 collector resolves a Secret name out of a CR, and
// rest-dynamic-controller's resolver later reads that Secret. If the two disagree about which element a
// predicate selects, the provider grants a Role for one Secret and the controller reads another -- #510's
// original failure mode, one level down.
//
// # On []Segment rather than []string
//
// rest-dynamic-controller's GetNestedField takes the flat form, so it re-derives intent from each
// segment's text: "0" might be an index or a map key, and it resolves them only because the DATA
// disambiguates. That is the content sniffing fieldpath's parser was built to remove, surviving one layer
// down.
//
// Taking []Segment lets the walker dispatch on Kind. Two consequences, one of them a deliberate
// correction:
//
//	KindIndex on a map    an error. The author wrote an index; a map has none.
//	KindKey on a slice     an error. a['0'] against an array is refused -- a CORRECTION, see the table.
//
// Get reads the value at segs from obj, which may nest through both map[string]interface{} and
// []interface{}.
//
// It keeps unstructured.NestedFieldNoCopy's three-way contract, which every caller of the function this
// replaces already depends on:
//
//	found=false, err=nil   the path does not resolve against this data. ABSENT.
//	err != nil             the path cannot APPLY to this data -- a kind mismatch, or a predicate that
//	                       matched more than one element. An instrument failure, not a fact about the
//	                       document, and callers must not collapse it into "absent".
//
// That distinction is the whole reason this signature is worth preserving: a caller that treats an error
// as absence turns "I could not read the field" into "the field is not there", which is how
// oasgen-provider #517 produced a controller that reported a resource missing while holding it.
func Get(obj any, segs []Segment) (any, bool, error) {
	cur := obj
	for i, seg := range segs {
		switch c := cur.(type) {
		case map[string]any:
			switch seg.Kind {
			case KindKey:
				v, ok := c[seg.Key]
				if !ok {
					return nil, false, nil
				}
				cur = v
			case KindIndex:
				return nil, false, fmt.Errorf("segment %d (%s): an array index cannot address a map; quote it as ['%d'] if it is a key", i, seg.String(), seg.Index)
			case KindPredicate:
				return nil, false, fmt.Errorf("segment %d (%s): a [?key=value] selector cannot address a map; it selects an element of an array", i, seg.String())
			default:
				return nil, false, fmt.Errorf("segment %d: unknown segment kind %v", i, seg.Kind)
			}
		case []any:
			switch seg.Kind {
			case KindIndex:
				if seg.Index < 0 || seg.Index >= len(c) {
					// Out of range is ABSENT, not an error: the path is applicable to an array, and
					// whether this array is long enough is a fact about the document. This matches
					// rest-dynamic-controller's behaviour, which callers already rely on.
					return nil, false, nil
				}
				cur = c[seg.Index]
			case KindPredicate:
				idx, found, err := matchPredicate(c, seg.Key, seg.Value)
				if err != nil {
					return nil, false, fmt.Errorf("segment %d (%s): %w", i, seg.String(), err)
				}
				if !found {
					return nil, false, nil
				}
				cur = c[idx]
			case KindKey:
				// A CORRECTION of the flat-form behaviour. With []string the walker could not tell
				// a['0'] from a[0], so a QUOTED key against an array silently indexed it. Dispatching on
				// Kind makes the author's intent decidable, and a key does not address an array element.
				return nil, false, fmt.Errorf("segment %d (%s): a map key cannot address an array element; use [%s] without quotes if you meant an index", i, seg.String(), seg.Key)
			default:
				return nil, false, fmt.Errorf("segment %d: unknown segment kind %v", i, seg.Kind)
			}
		default:
			return nil, false, fmt.Errorf("segment %d (%s): %T is neither a map nor an array", i, seg.String(), cur)
		}
	}
	return cur, true, nil
}

// matchPredicate resolves a [?key=value] selector against a slice, returning the index of the single
// matching element. PORTED FROM rest-dynamic-controller UNCHANGED, including both rules below, because a
// re-specification would be a new dialect rather than a shared one.
//
// Comparison is by STRING FORM, so a selector matches whether the document holds "2", 2 or 2.0 -- API
// bodies round-trip through JSON with inconsistent numeric typing, and a type-sensitive match would fail
// for reasons the author cannot see in their YAML.
//
// AMBIGUITY IS AN ERROR, never "take the first". Silently picking one of several matches is exactly the
// wrong-element bug selectors exist to eliminate.
func matchPredicate(items []any, key, want string) (int, bool, error) {
	hit := -1
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue // a scalar element cannot satisfy a field selector
		}
		v, present := m[key]
		if !present {
			continue
		}
		if fmt.Sprintf("%v", v) != want {
			continue
		}
		if hit >= 0 {
			return 0, false, fmt.Errorf("selector [?%s=%s] matches more than one element (indices %d and %d); it must select exactly one", key, want, hit, i)
		}
		hit = i
	}
	if hit < 0 {
		return 0, false, nil
	}
	return hit, true, nil
}
