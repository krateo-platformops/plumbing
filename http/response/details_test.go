package response

// details_test.go: Status.Details, the optional extended data of a Status.
//
//   - StatusDetails and StatusCause mirror metav1's, field for field and JSON tag for JSON tag;
//   - a Status with Details is, on the wire, a metav1.Status: it round-trips through one in both
//     directions without loss;
//   - Encode and AsMap carry Details when set;
//   - no constructor sets it (the golden pins the bodies; this pins the field).

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// jsonShape is a struct's fields as JSON sees them: name, JSON tag, and the kind of the Go type
// (so types.UID and string, or metav1.CauseType and CauseType, compare equal).
func jsonShape(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		ft := f.Type
		shape := ft.Kind().String()
		if ft.Kind() == reflect.Slice || ft.Kind() == reflect.Pointer {
			shape += " of " + ft.Elem().Kind().String()
		}
		out = append(out, f.Name+" `json:\""+f.Tag.Get("json")+"\"` "+shape)
	}
	return out
}

func TestStatusDetails_MirrorsMetaV1(t *testing.T) {
	for _, c := range []struct {
		name       string
		ours, meta reflect.Type
	}{
		{"StatusDetails", reflect.TypeOf(StatusDetails{}), reflect.TypeOf(metav1.StatusDetails{})},
		{"StatusCause", reflect.TypeOf(StatusCause{}), reflect.TypeOf(metav1.StatusCause{})},
	} {
		got, want := jsonShape(c.ours), jsonShape(c.meta)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s does not mirror metav1.%s\n got: %s\nwant: %s", c.name, c.name, strings.Join(got, "; "), strings.Join(want, "; "))
		}
	}
	// The field on Status itself: same name, same tag, a pointer, as metav1.Status has it.
	ours, _ := reflect.TypeOf(Status{}).FieldByName("Details")
	meta, _ := reflect.TypeOf(metav1.Status{}).FieldByName("Details")
	if ours.Tag.Get("json") != meta.Tag.Get("json") || ours.Type.Kind() != reflect.Pointer {
		t.Errorf("Status.Details is `json:%q` %s, metav1's is `json:%q` %s",
			ours.Tag.Get("json"), ours.Type.Kind(), meta.Tag.Get("json"), meta.Type.Kind())
	}
}

// fullDetails sets every field of StatusDetails and StatusCause.
func fullDetails() *StatusDetails {
	return &StatusDetails{
		Name: "x", Group: "g.example.org", Kind: "things", UID: "0000-1111",
		Causes: []StatusCause{
			{Type: "TypeNotInstalled", Message: "g.example.org/v1", Field: "apiVersion"},
			{Type: "FieldValueInvalid", Message: "bad", Field: "spec.x[0].y"},
		},
		RetryAfterSeconds: 7,
	}
}

func TestStatusDetails_OursDecodesAsAMetaV1Status(t *testing.T) {
	ours := &Status{Kind: "Status", APIVersion: "v1", Status: StatusFailure, Message: "m", Reason: "FailedDependency", Details: fullDetails(), Code: 424}
	body, err := json.Marshal(ours)
	if err != nil {
		t.Fatal(err)
	}
	const wantBody = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"m","reason":"FailedDependency",` +
		`"details":{"name":"x","group":"g.example.org","kind":"things","uid":"0000-1111",` +
		`"causes":[{"reason":"TypeNotInstalled","message":"g.example.org/v1","field":"apiVersion"},{"reason":"FieldValueInvalid","message":"bad","field":"spec.x[0].y"}],` +
		`"retryAfterSeconds":7},"code":424}`
	if string(body) != wantBody {
		t.Fatalf("body\n got: %s\nwant: %s", body, wantBody)
	}

	var meta metav1.Status
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatalf("a client-go reader cannot decode it: %v", err)
	}
	want := metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure, Message: "m", Reason: "FailedDependency", Code: 424,
		Details: &metav1.StatusDetails{
			Name: "x", Group: "g.example.org", Kind: "things", UID: "0000-1111",
			Causes: []metav1.StatusCause{
				{Type: "TypeNotInstalled", Message: "g.example.org/v1", Field: "apiVersion"},
				{Type: "FieldValueInvalid", Message: "bad", Field: "spec.x[0].y"},
			},
			RetryAfterSeconds: 7,
		},
	}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("decoded as metav1.Status\n got: %+v\nwant: %+v", meta, want)
	}

	// And back: the metav1.Status, encoded, decodes into the Status we started from.
	back, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	var again Status
	if err := json.Unmarshal(back, &again); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&again, ours) {
		t.Fatalf("round trip through metav1.Status\n got: %+v\nwant: %+v", again, *ours)
	}
}

// The other direction, from the Status values the apiserver's own error constructors build: each
// decodes here and encodes back to a metav1.Status equal to the original.
func TestStatusDetails_AMetaV1StatusRoundTripsThroughOurs(t *testing.T) {
	gr := schema.GroupResource{Group: "g.example.org", Resource: "things"}
	for name, serr := range map[string]*apierrors.StatusError{
		"not_found":         apierrors.NewNotFound(gr, "x"),
		"forbidden":         apierrors.NewForbidden(gr, "x", errors.New("no")),
		"conflict":          apierrors.NewConflict(gr, "x", errors.New("changed")),
		"too_many_requests": apierrors.NewTooManyRequests("slow down", 9),
		"invalid": apierrors.NewInvalid(schema.GroupKind{Group: "g.example.org", Kind: "Thing"}, "x", field.ErrorList{
			field.Invalid(field.NewPath("spec", "x"), "v", "bad"),
			field.Required(field.NewPath("spec", "y"), "needed"),
		}),
		"unauthorized_no_details": apierrors.NewUnauthorized("who"),
	} {
		t.Run(name, func(t *testing.T) {
			orig := serr.Status()
			body, err := json.Marshal(orig)
			if err != nil {
				t.Fatal(err)
			}
			var ours Status
			if err := json.Unmarshal(body, &ours); err != nil {
				t.Fatalf("decode into Status: %v", err)
			}
			if (ours.Details == nil) != (orig.Details == nil) {
				t.Fatalf("Details present=%v, the metav1.Status has Details present=%v", ours.Details != nil, orig.Details != nil)
			}
			back, err := json.Marshal(ours)
			if err != nil {
				t.Fatal(err)
			}
			var again metav1.Status
			if err := json.Unmarshal(back, &again); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(again, orig) {
				t.Fatalf("round trip through Status\n got: %+v\nwant: %+v\nbody: %s", again, orig, back)
			}
		})
	}
}

func TestStatusDetails_EncodeAndAsMapCarryThem(t *testing.T) {
	st := &Status{Kind: "Status", APIVersion: "v1", Status: StatusFailure, Message: "m", Reason: "FailedDependency", Code: http.StatusFailedDependency,
		Details: &StatusDetails{Group: "g.example.org", Kind: "things",
			Causes: []StatusCause{{Type: "TypeNotInstalled", Message: "g.example.org/v1", Field: "apiVersion"}}}}

	rec := httptest.NewRecorder()
	if err := Encode(rec, st); err != nil {
		t.Fatal(err)
	}
	const want = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"m","reason":"FailedDependency",` +
		`"details":{"group":"g.example.org","kind":"things","causes":[{"reason":"TypeNotInstalled","message":"g.example.org/v1","field":"apiVersion"}]},"code":424}` + "\n"
	if rec.Code != http.StatusFailedDependency || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != want {
		t.Fatalf("Encode wrote %d %q\n got: %s\nwant: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String(), want)
	}
	// Encode does not touch the Status it is given.
	if st.Details == nil || len(st.Details.Causes) != 1 {
		t.Fatalf("Encode changed the Status's Details")
	}

	m, err := AsMap(st)
	if err != nil {
		t.Fatal(err)
	}
	wantDetails := map[string]any{"group": "g.example.org", "kind": "things",
		"causes": []any{map[string]any{"reason": "TypeNotInstalled", "message": "g.example.org/v1", "field": "apiVersion"}}}
	if !reflect.DeepEqual(m["details"], wantDetails) {
		t.Fatalf("AsMap details\n got: %#v\nwant: %#v", m["details"], wantDetails)
	}
}

// An empty, non-nil Details is still the caller's: it is encoded as an empty object, not dropped.
// Only a nil Details is omitted.
func TestStatusDetails_OnlyNilIsOmitted(t *testing.T) {
	body, err := json.Marshal(Status{Code: 424, Details: &StatusDetails{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"details":{},"code":424}` {
		t.Fatalf("got %s", body)
	}
	body, _ = json.Marshal(Status{Code: 424})
	if string(body) != `{"code":424}` {
		t.Fatalf("a nil Details must be omitted, got %s", body)
	}
}

func TestStatusDetails_NoConstructorSetsThem(t *testing.T) {
	for code := 0; code < 600; code++ {
		if st := New(code, errors.New("e")); st.Details != nil {
			t.Fatalf("New(%d) set Details", code)
		}
	}
}
