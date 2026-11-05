package crdgen

import (
	"math"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/krateo-platformops/plumbing/crdgen/schemas"
)

// core-provider#362. Each warning carries a Kind, so a caller acts on what a warning MEANS rather
// than on its text. One schema per kind that can be produced from JSON input.
func TestGenerateWithDiagnostics_Kinds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     Options
		wantKind WarningKind
		wantPath string
		loses    bool
	}{
		{"unsupported not -> constraint dropped",
			Options{SpecSchema: []byte(`{"type":"object","properties":{"o":{"type":"object","not":{"required":["a"]}}}}`)},
			KindConstraintDropped, "spec.o", true},
		{"$ref cycle -> structure loosened",
			Options{SpecSchema: []byte(`{"type":"object","$defs":{"n":{"type":"object","properties":{"next":{"$ref":"#/$defs/n"}}}},"properties":{"root":{"$ref":"#/$defs/n"}}}`)},
			KindStructureLoosened, "", true},
		{"union type -> acceptance narrowed",
			Options{SpecSchema: []byte(`{"type":"object","properties":{"u":{"type":["string","boolean"]}}}`)},
			KindAcceptanceNarrowed, "spec.u", false},
		{"optional parent stamped -> parent defaulted",
			Options{SpecSchema: []byte(`{"type":"object","properties":{"img":{"type":"object","properties":{"tag":{"type":"string","default":"x"}}}}}`)},
			KindParentDefaulted, "spec.img", false},
		{"observedGeneration declared as string -> convention conflict",
			Options{SpecSchema: []byte(`{"type":"object"}`), Managed: true,
				StatusSchema: []byte(`{"type":"object","properties":{"observedGeneration":{"type":"string"}}}`)},
			KindConventionConflict, "status.observedGeneration", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Group, tc.opts.Version, tc.opts.Kind = "x.io", "v1", "K"
			_, ws, err := GenerateWithDiagnostics(tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			var hit *Warning
			for i := range ws {
				if ws[i].Kind == tc.wantKind {
					hit = &ws[i]
					break
				}
			}
			if hit == nil {
				t.Fatalf("no %s warning; got %+v", tc.wantKind, ws)
			}
			if tc.wantPath != "" && hit.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", hit.Path, tc.wantPath)
			}
			if hit.Kind.LosesEnforcement() != tc.loses {
				t.Errorf("%s.LosesEnforcement() = %v, want %v", hit.Kind, hit.Kind.LosesEnforcement(), tc.loses)
			}
		})
	}
}

// GenerateWithWarnings must stay exactly the String() of the typed warnings, in order, and keep
// nil-vs-empty: nil only when the transpiler never ran.
func TestGenerateWithWarnings_IsTheStringOfTheTypedWarnings(t *testing.T) {
	opts := Options{Group: "x.io", Version: "v1", Kind: "K", Managed: true,
		SpecSchema:   []byte(`{"type":"object","properties":{"u":{"type":["string","boolean"]},"img":{"type":"object","properties":{"tag":{"type":"string","default":"x"}}}}}`),
		StatusSchema: []byte(`{"type":"object","properties":{"observedGeneration":{"type":"string"}}}`)}
	_, typed, err1 := GenerateWithDiagnostics(opts)
	_, plain, err2 := GenerateWithWarnings(opts)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if len(typed) == 0 || len(typed) != len(plain) {
		t.Fatalf("typed=%d plain=%d: they must be the same warnings", len(typed), len(plain))
	}
	for i := range typed {
		if typed[i].String() != plain[i] {
			t.Errorf("warning %d: %q != %q", i, typed[i].String(), plain[i])
		}
	}
	if (Warning{Message: "m"}).String() != "m" || (Warning{Path: "p", Message: "m"}).String() != "p: m" {
		t.Error("String() must be \"path: message\", or the message alone without a path")
	}

	_, ws, err := GenerateWithWarnings(Options{SpecSchema: []byte(`{not json`)})
	if err == nil || ws != nil {
		t.Errorf("a parse failure must return nil warnings (never checked), got %v (err %v)", ws, err)
	}
}

// The enum arm cannot be reached from JSON input (a parsed JSON value always marshals back), so it
// is driven directly with a NaN, which json.Marshal refuses. Its kind depends on what is left.
func TestCopyEnum_KindDependsOnWhatIsLeft(t *testing.T) {
	for _, tc := range []struct {
		name string
		enum []interface{}
		want WarningKind
	}{
		{"some values carried: the dropped one is refused", []interface{}{"a", math.NaN()}, KindAcceptanceNarrowed},
		{"no value carried: no enum, anything accepted", []interface{}{math.NaN()}, KindConstraintDropped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &transpiler{}
			tr.copyEnum(&apiextensionsv1.JSONSchemaProps{Type: "string"}, &schemas.Type{Enum: tc.enum}, "spec.e")
			if len(tr.warnings) != 1 || tr.warnings[0].Kind != tc.want {
				t.Fatalf("want one %s warning, got %+v", tc.want, tr.warnings)
			}
		})
	}
}

func TestCopyDefault_UnmarshalableIsDefaultDropped(t *testing.T) {
	tr := &transpiler{}
	tr.copyDefault(&apiextensionsv1.JSONSchemaProps{Type: "number"}, &schemas.Type{Default: math.NaN()}, "spec.d")
	if len(tr.warnings) != 1 || tr.warnings[0].Kind != KindDefaultDropped || tr.warnings[0].Kind.LosesEnforcement() {
		t.Fatalf("want one DefaultDropped warning that does not lose enforcement, got %+v", tr.warnings)
	}
}
