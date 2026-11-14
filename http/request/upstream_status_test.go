package request

// upstream_status_test.go pins what Do returns for an upstream's error body, as JSON: the
// envelope's kind, apiVersion, status, message, reason and code, and nothing else.
//
// response.Status gained a Details field. Do decodes an upstream body into a Status, so without
// care an upstream's "details" (an apiserver's carries the object's name and uid) would start
// appearing in what Do returns, and a body whose "details" is not an object would stop decoding
// and fall back to the raw text. Neither may happen: every row here passes, unchanged, on the
// package as it was before the field existed (commit 0012dac7e69dd4fad52e45799751de997d08a461).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/ptr"
)

func TestDo_AnUpstreamErrorBodyIsReturnedWithoutItsDetails(t *testing.T) {
	for _, c := range []struct {
		name string
		code int
		body string
		want string
	}{
		{
			// An apiserver's NotFound, as it is on the wire.
			name: "apiserver_not_found_with_details",
			code: http.StatusNotFound,
			body: `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"configmaps \"x\" not found","reason":"NotFound","details":{"name":"x","kind":"configmaps"},"code":404}`,
			want: `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"configmaps \"x\" not found","reason":"NotFound","code":404}`,
		},
		{
			name: "invalid_with_causes_and_uid",
			code: http.StatusUnprocessableEntity,
			body: `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"invalid","reason":"Invalid","details":{"name":"x","group":"g.example.org","kind":"Thing","uid":"0000-1111","causes":[{"reason":"FieldValueInvalid","message":"bad","field":"spec.x"}],"retryAfterSeconds":3},"code":422}`,
			want: `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"invalid","reason":"Invalid","code":422}`,
		},
		{
			name: "no_details",
			code: http.StatusForbidden,
			body: `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"forbidden","reason":"Forbidden","code":403}`,
			want: `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"forbidden","reason":"Forbidden","code":403}`,
		},
		{
			// "details" that is not an object: the envelope still decodes, as it always did.
			name: "details_is_a_string",
			code: http.StatusTeapot,
			body: `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"upstream said no","reason":"Conflict","details":"see the log","code":409}`,
			want: `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"upstream said no","reason":"Conflict","code":409}`,
		},
		{
			name: "details_is_an_array",
			code: http.StatusBadRequest,
			body: `{"status":"Failure","message":"m","details":[1,2],"code":400}`,
			want: `{"status":"Failure","message":"m","code":400}`,
		},
		{
			name: "details_is_null",
			code: http.StatusBadRequest,
			body: `{"status":"Failure","message":"m","details":null,"code":400}`,
			want: `{"status":"Failure","message":"m","code":400}`,
		},
		{
			// JSON that isn't a Status: never an empty Status with code 0
			// (plumbing#174), but the response's own code, like a body that
			// doesn't decode.
			name: "json_that_is_not_a_status",
			code: http.StatusUnprocessableEntity,
			body: `{"error":"boom"}`,
			want: `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"{\"error\":\"boom\"}","reason":"UnprocessableEntity","code":422}`,
		},
		{
			name: "json_null",
			code: http.StatusUnprocessableEntity,
			body: `null`,
			want: `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"null","reason":"UnprocessableEntity","code":422}`,
		},
		{
			// Not JSON at all: the raw text under the response's code, as before.
			name: "not_json",
			code: http.StatusTeapot,
			body: `upstream connect error`,
			want: `{"kind":"Status","apiVersion":"v1","status":"Success","message":"upstream connect error","code":418}`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.code)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			st := Do(context.Background(), RequestOptions{
				RequestInfo: RequestInfo{Path: "/x", Verb: ptr.To(http.MethodGet)},
				Endpoint:    &endpoints.Endpoint{ServerURL: srv.URL},
			})
			if st == nil {
				t.Fatalf("Do returned nil")
			}
			got, err := json.Marshal(st)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("Do returned\n got: %s\nwant: %s", got, c.want)
			}
		})
	}
}
