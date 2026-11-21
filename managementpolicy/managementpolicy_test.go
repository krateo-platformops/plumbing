package managementpolicy

import (
	"strconv"
	"strings"
	"testing"
)

// The presets keep their historical meaning (D1), except the one case D5 flips.
func TestParse_PresetsAreAliases(t *testing.T) {
	for value, want := range map[string]string{
		"":                      "observe,create,update,delete",
		"default":               "observe,create,update,delete",
		"observe-create-update": "observe,create,update",
		"observe-delete":        "observe,delete",
		"observe":               "observe",
	} {
		p := Parse(value)
		if p.Invalid != "" || p.String() != want {
			t.Errorf("Parse(%q) = %q (invalid %q), want %q", value, p, p.Invalid, want)
		}
	}
}

// D1 and D3: a set, any order, whitespace tolerated, observe implied.
func TestParse_Sets(t *testing.T) {
	for value, want := range map[string]string{
		"observe,create":         "observe,create",
		"create":                 "observe,create", // D3
		"create, observe":        "observe,create",
		" update ,delete ":       "observe,update,delete",
		"create,update,delete":   "observe,create,update,delete",
		"observe,observe,create": "observe,create",
	} {
		p := Parse(value)
		if p.Invalid != "" || p.String() != want {
			t.Errorf("Parse(%q) = %q (invalid %q), want %q", value, p, p.Invalid, want)
		}
	}
}

// D2: anything that does not parse acts on NOTHING (observe only) and says what was wrong. A typo
// meant to restrict must never widen.
func TestParse_InvalidActsOnNothing(t *testing.T) {
	for value, mustName := range map[string]string{
		"observe-create":  `"observe-create"`, // the typo aruba's table measured as silently partial
		"observe,creat":   `"creat"`,
		"Create":          `"Create"`, // tokens are lowercase
		"observe,,create": "empty entry",
		"observe,":        "empty entry",
		"all":             `"all"`,
	} {
		p := Parse(value)
		if p.Invalid == "" {
			t.Errorf("Parse(%q) is valid (%q); want invalid", value, p)
			continue
		}
		if p.String() != "observe" {
			t.Errorf("Parse(%q) = %q; an invalid value must allow observe only", value, p)
		}
		if !strings.Contains(p.Invalid, mustName) {
			t.Errorf("Parse(%q).Invalid = %q; want it to name %s", value, p.Invalid, mustName)
		}
	}
}

// The full table: every subset of {create, update, delete} x deletion-policy {unset, delete, orphan,
// an unrecognised value} x
// declared {nil (no information), all, none, create-only}. Each row's expectation is computed from
// the decided rules written out here, independently of Why's implementation order:
//   - observe always may;
//   - a verb not declared (when declared is given) is NotDeclared;
//   - else a verb not in the set is ByPolicy;
//   - else delete with orphan, or with an unrecognised deletion-policy, is ByDeletionPolicy (D5: orphan
//     always wins; an unknown value fails closed);
//   - else it may run.
func TestDecision_TruthTable(t *testing.T) {
	subsets := []string{"observe", "observe,create", "observe,update", "observe,delete",
		"observe,create,update", "observe,create,delete", "observe,update,delete", "observe,create,update,delete"}
	declareds := map[string][]Verb{
		"nil":    nil,
		"all":    {Observe, Create, Update, Delete},
		"none":   {},
		"create": {Observe, Create},
	}
	deletions := []struct {
		value string // "" = annotation absent
		keeps bool
	}{{"", false}, {"delete", false}, {"orphan", true}, {"orphaned", true}}
	rows := 0
	for _, set := range subsets {
		for _, del := range deletions {
			orphan := del.keeps
			for dname, declared := range declareds {
				ann := map[string]string{AnnotationKey: set}
				if del.value != "" {
					ann[DeletionAnnotationKey] = del.value
				}
				d := For(ann, declared)
				for _, v := range verbs {
					want := NotWithheld
					inSet := strings.Contains(","+set+",", ","+string(v)+",")
					declaredHas := declared == nil
					for _, dv := range declared {
						declaredHas = declaredHas || dv == v
					}
					switch {
					case v == Observe:
					case !declaredHas:
						want = NotDeclared
					case !inSet:
						want = ByPolicy
					case v == Delete && orphan:
						want = ByDeletionPolicy
					}
					if got := d.Why(v); got != want {
						t.Errorf("set=%q deletion=%q declared=%s: Why(%s) = %q, want %q", set, del.value, dname, v, got, want)
					}
					rows++
				}
			}
		}
	}
	if rows != 8*4*4*4 {
		t.Fatalf("checked %d (set, deletion, declared, verb) rows, want %d", rows, 8*4*4*4)
	}
}

// D5, the one documented behaviour that changes: observe-delete used to delete despite
// deletion-policy: orphan (oasgen-provider docs/log.md, #609/#611/#616). Orphan now always wins.
// CONTROL: observe-delete WITHOUT orphan still deletes.
func TestDecision_OrphanAlwaysWins(t *testing.T) {
	if d := For(map[string]string{AnnotationKey: "observe-delete", DeletionAnnotationKey: "orphan"}, nil); d.May(Delete) {
		t.Error("observe-delete + orphan deletes; D5 says orphan always wins")
	}
	if d := For(map[string]string{AnnotationKey: "observe-delete"}, nil); !d.May(Delete) {
		t.Error("control: observe-delete without orphan must still delete")
	}
	if d := For(map[string]string{DeletionAnnotationKey: "orphan"}, nil); d.May(Delete) || !d.May(Update) {
		t.Error("default + orphan: delete withheld, everything else allowed")
	}
}

// D8: declared absent (nil) is "no information", declared empty is "declares nothing". Collapsing
// them would make every handler that predates the declared-verbs interface reconcile nothing.
func TestDecision_DeclaredAbsentIsNotDeclaredEmpty(t *testing.T) {
	absent := For(nil, nil)
	empty := For(nil, []Verb{})
	if !absent.May(Create) || !absent.May(Delete) {
		t.Error("declared absent must leave the policy alone deciding (default: everything)")
	}
	if empty.May(Create) || empty.Why(Update) != NotDeclared || !empty.May(Observe) {
		t.Errorf("declared empty: create/update/delete NotDeclared, observe allowed; got create=%v update=%q", empty.May(Create), empty.Why(Update))
	}
}

// An invalid policy withholds every write, whatever else is set.
func TestDecision_InvalidWithholdsEveryWrite(t *testing.T) {
	d := For(map[string]string{AnnotationKey: "observe-creat"}, nil)
	if d.Policy.Invalid == "" {
		t.Fatal("premise: the value must be invalid")
	}
	if got := d.Withholds(); len(got) != 3 || got[0] != Create || got[1] != Update || got[2] != Delete {
		t.Errorf("Withholds = %v, want [create update delete]", got)
	}
}

// authn's review of #220 (Diego, 2026-11-19): an unrecognised deletion-policy KEEPS the resource and
// says so, whatever the management policy allows. CONTROL: "" and "delete" still delete under default.
func TestDecision_UnknownDeletionPolicyKeeps(t *testing.T) {
	for _, v := range []string{"orphaned", "Orphan", "keep", "Delete", "true"} {
		d := For(map[string]string{DeletionAnnotationKey: v}, nil)
		if d.May(Delete) || d.Why(Delete) != ByDeletionPolicy {
			t.Errorf("deletion-policy %q: May(Delete)=%v Why=%q; want it withheld", v, d.May(Delete), d.Why(Delete))
		}
		if !strings.Contains(d.DeletionInvalid, strconv.Quote(v)) {
			t.Errorf("deletion-policy %q: DeletionInvalid = %q; want it to name the value", v, d.DeletionInvalid)
		}
	}
	for _, v := range []string{"", "delete", " delete "} {
		ann := map[string]string{}
		if v != "" {
			ann[DeletionAnnotationKey] = v
		}
		if d := For(ann, nil); !d.May(Delete) || d.DeletionInvalid != "" {
			t.Errorf("control: deletion-policy %q must delete under default, with no invalid report (got May=%v, invalid %q)", v, d.May(Delete), d.DeletionInvalid)
		}
	}
	if d := For(map[string]string{DeletionAnnotationKey: "orphan"}, nil); d.DeletionInvalid != "" {
		t.Errorf("orphan is a recognised value, not invalid: %q", d.DeletionInvalid)
	}
}
