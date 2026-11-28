package apiref

import (
	"reflect"
	"testing"
)

func TestCompositionExtras_NilStaticHasExactlyTheThreeKeys(t *testing.T) {
	got := CompositionExtras(nil, "c1", "ns1", "uid-1")
	want := map[string]any{
		"compositionName":      "c1",
		"compositionNamespace": "ns1",
		"compositionId":        "uid-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCompositionExtras_PerInstanceContextWins(t *testing.T) {
	static := map[string]any{"region": "eu", "compositionName": "spoof"}
	got := CompositionExtras(static, "real", "ns1", "uid-1")
	if got["region"] != "eu" {
		t.Errorf("region = %v, want eu (static extras are kept)", got["region"])
	}
	if got["compositionName"] != "real" {
		t.Errorf("compositionName = %v, want real (per-instance wins)", got["compositionName"])
	}
	if len(got) != 4 {
		t.Errorf("len = %d, want 4: %v", len(got), got)
	}
}

func TestCompositionExtras_DoesNotMutateStatic(t *testing.T) {
	static := map[string]any{"region": "eu", "compositionName": "spoof"}
	before := map[string]any{"region": "eu", "compositionName": "spoof"}
	got := CompositionExtras(static, "real", "ns1", "uid-1")
	if !reflect.DeepEqual(static, before) {
		t.Fatalf("static mutated: %v, want %v", static, before)
	}
	got["region"] = "us"
	if static["region"] != "eu" {
		t.Fatalf("result aliases static")
	}
}

func TestCompositionExtras_EmptyUIDKeyPresent(t *testing.T) {
	got := CompositionExtras(nil, "c1", "ns1", "")
	v, ok := got["compositionId"]
	if !ok || v != "" {
		t.Fatalf("compositionId = %v (present=%v), want present with empty string", v, ok)
	}
}
