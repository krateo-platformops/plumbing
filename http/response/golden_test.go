package response

// golden_test.go pins, byte for byte, every body this package produces for a Status that carries
// no Details: every constructor (New, for every status code it knows and several it does not,
// with and without an error), every typed writer through Encode, a hand-built Status, the zero
// Status, and AsMap.
//
// testdata/bodies.golden was generated from the package as it was BEFORE Status gained the
// Details field (commit 0012dac7e69dd4fad52e45799751de997d08a461), so this test is the proof that
// adding the field changed no existing body. Regenerate with UPDATE_GOLDEN=1 only when a body is
// meant to change.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const goldenPath = "testdata/bodies.golden"

// goldenBodies is every body, one "name<TAB>bytes" line each, in a fixed order.
func goldenBodies(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	line := func(name string, body []byte) {
		fmt.Fprintf(&b, "%s\t%s\n", name, strings.TrimSuffix(string(body), "\n"))
	}
	marshal := func(v any) []byte {
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return out
	}

	// New: every code its switch names, and codes it does not.
	codes := []int{
		http.StatusUnprocessableEntity, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusConflict, http.StatusGone, http.StatusNotImplemented, http.StatusBadRequest,
		http.StatusServiceUnavailable, http.StatusNotAcceptable, http.StatusMethodNotAllowed,
		http.StatusInternalServerError, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType,
		0, http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusFound, http.StatusTeapot,
		http.StatusFailedDependency, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusGatewayTimeout,
	}
	for _, code := range codes {
		line(fmt.Sprintf("New(%d,nil)", code), marshal(New(code, nil)))
		line(fmt.Sprintf("New(%d,err)", code), marshal(New(code, errors.New(`boom "quoted" <tag> & more`))))
	}

	// The typed writers, through Encode: status line, Content-Type and body.
	writers := []struct {
		name string
		fn   func(http.ResponseWriter, error) error
	}{
		{"Unauthorized", Unauthorized}, {"InternalError", InternalError}, {"ServiceUnavailable", ServiceUnavailable},
		{"BadRequest", BadRequest}, {"NotAcceptable", NotAcceptable}, {"MethodNotAllowed", MethodNotAllowed},
		{"NotFound", NotFound}, {"Forbidden", Forbidden},
	}
	for _, w := range writers {
		for _, err := range []error{nil, errors.New("the reason")} {
			rec := httptest.NewRecorder()
			if werr := w.fn(rec, err); werr != nil {
				t.Fatalf("%s: %v", w.name, werr)
			}
			line(fmt.Sprintf("%s(err=%v) %d %s", w.name, err != nil, rec.Code, rec.Header().Get("Content-Type")), rec.Body.Bytes())
		}
	}

	// Encode of a Status built by hand, as a consumer builds one for a code New does not know.
	hand := &Status{Kind: "Status", APIVersion: "v1", Status: StatusFailure, Message: "a dependency is missing", Reason: "FailedDependency", Code: http.StatusFailedDependency}
	rec := httptest.NewRecorder()
	if err := Encode(rec, hand); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	line(fmt.Sprintf("Encode(hand-built) %d %s", rec.Code, rec.Header().Get("Content-Type")), rec.Body.Bytes())

	// The zero Status, a partial one, and AsMap.
	line("Marshal(Status{})", marshal(Status{}))
	line("Marshal(partial)", marshal(Status{Status: StatusFailure, Code: 500}))
	m, err := AsMap(hand)
	if err != nil {
		t.Fatalf("AsMap: %v", err)
	}
	line("AsMap(hand-built)", marshal(m))
	m, err = AsMap(&Status{})
	if err != nil {
		t.Fatalf("AsMap: %v", err)
	}
	line("AsMap(Status{})", marshal(m))
	return b.String()
}

func TestGolden_EveryBodyWithoutDetailsIsByteIdentical(t *testing.T) {
	got := goldenBodies(t)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", goldenPath, len(got))
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got == string(want) {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Errorf("body %d differs\n got: %s\nwant: %s", i+1, g, w)
		}
	}
}
