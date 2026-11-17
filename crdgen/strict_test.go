package crdgen

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func genStrict(schema string, strict bool) ([]byte, []Warning, error) {
	return GenerateWithDiagnostics(Options{Group: "composition.krateo.io", Version: "v1", Kind: "Probe", SpecSchema: []byte(schema), Strict: strict})
}

// plumbing#48: with Options.Strict, a restriction crdgen would drop fails generation, naming the path
// and the keyword. Without it, the same schema generates, with the warning (today's behaviour).
func TestStrictRefusesADroppedRestriction(t *testing.T) {
	const schema = `{"type":"object","properties":{"l":{"type":"array","maxItems":4,"items":{"type":"string"},"not":{"maxItems":1}}}}`

	_, ws, err := genStrict(schema, false)
	if err != nil {
		t.Fatalf("without Strict the schema must generate: %v", err)
	}
	var dropped int
	for _, w := range ws {
		if w.Kind == KindConstraintDropped {
			dropped++
		}
	}
	if dropped != 1 {
		t.Fatalf("the fixture must drop exactly one restriction, got %v", ws)
	}

	_, _, err = genStrict(schema, true)
	var se *StrictError
	if !errors.As(err, &se) || len(se.Dropped) != 1 || se.Dropped[0].Path != "spec.l" {
		t.Fatalf("want a *StrictError naming spec.l, got %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "spec.l: not on non-scalar node unsupported") || !strings.Contains(msg, "Options.Strict") {
		t.Errorf("the error does not name the path and the keyword: %s", msg)
	}
}

// Strict refuses restrictions only. Structure loosened to preserve-unknown is #48's "fine as a
// warning" case, and a dropped default is not a restriction.
func TestStrictLetsLoosenedStructureThrough(t *testing.T) {
	_, ws, err := genStrict(`{"type":"object","properties":{"r":{"$ref":"#/$defs/Missing"}}}`, true)
	if err != nil {
		t.Fatalf("a loosened structure must not fail Strict: %v", err)
	}
	loosened := false
	for _, w := range ws {
		loosened = loosened || w.Kind == KindStructureLoosened
	}
	if !loosened {
		t.Fatal("the fixture must loosen structure, or this test proves nothing")
	}
}

// Strict changes nothing for a schema that loses nothing: the same CRD, byte for byte. And the
// translation in the same change is what makes core-provider's own chart pass it.
func TestStrictIsANoOpWhenNothingIsDropped(t *testing.T) {
	for _, schema := range []string{envForbidsKeys, identityShape} {
		lax, _, err := genStrict(schema, false)
		if err != nil {
			t.Fatal(err)
		}
		strict, _, err := genStrict(schema, true)
		if err != nil {
			t.Fatalf("Strict refused a schema that drops nothing: %v", err)
		}
		if !bytes.Equal(lax, strict) {
			t.Error("Strict changed the generated CRD")
		}
	}
}

// A dropped generated uniqueness rule is a dropped restriction too (uniqueItems not enforced), and
// it is reported without a path, for the CRD as a whole; Strict refuses it like any other.
func TestStrictRefusesADroppedUniquenessRule(t *testing.T) {
	_, _, err := genStrict(string(nestedUniqueSpec(0)), true)
	var se *StrictError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "uniqueness rule") {
		t.Fatalf("want the dropped uniqueness rule refused under Strict, got %v", err)
	}
}
