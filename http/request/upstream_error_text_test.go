package request

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/ptr"
)

// upstreamErrorText (plumbing#175): a body that is JSON is never echoed raw;
// a JSON object answers its non-empty string "message", every other JSON body
// "upstream answered <code>"; a body that isn't JSON is echoed. A body cut at
// the size limit that starts like JSON is JSON. snowplow's two error decoders
// mirror this table row for row.
func TestUpstreamErrorText(t *testing.T) {
	cutObject := `{"details":{"uid":"SECRET"},"pad":"` + strings.Repeat("x", maxUnstructuredResponseTextBytes) // cut: invalid JSON
	cutText := strings.Repeat("y", maxUnstructuredResponseTextBytes)
	for _, c := range []struct {
		name      string
		body      string
		truncated bool
		want      string
	}{
		{name: "plain text is echoed", body: `upstream connect error`, want: `upstream connect error`},
		{name: "HTML is echoed", body: `<html><body>502 Bad Gateway</body></html>`, want: `<html><body>502 Bad Gateway</body></html>`},
		{name: "an empty body is echoed (empty)", body: ``, want: ``},
		{name: "plain text cut at the limit is echoed", body: cutText, truncated: true, want: cutText},
		{name: "an object's message", body: `{"message":"Bad credentials","uid":"SECRET"}`, want: `Bad credentials`},
		{name: "an object's message, surrounded by spaces", body: "  \n{\"message\":\"Bad credentials\"}\n", want: `Bad credentials`},
		{name: "an object without a message", body: `{"error":"boom","uid":"SECRET"}`, want: `upstream answered 422`},
		{name: "an object with an empty message", body: `{"message":""}`, want: `upstream answered 422`},
		{name: "an object whose message isn't a string", body: `{"message":{"uid":"SECRET"}}`, want: `upstream answered 422`},
		{name: "a Status-shaped object that fails to decode", body: `{"status":"Failure","code":"x","details":{"uid":"SECRET"}}`, want: `upstream answered 422`},
		{name: "an array", body: `[{"uid":"SECRET"}]`, want: `upstream answered 422`},
		{name: "null", body: `null`, want: `upstream answered 422`},
		{name: "a number", body: `42`, want: `upstream answered 422`},
		{name: "a JSON string", body: `"SECRET"`, want: `upstream answered 422`},
		{name: "an object cut at the limit", body: cutObject[:maxUnstructuredResponseTextBytes], truncated: true, want: `upstream answered 422`},
		{name: "an array cut at the limit", body: `[` + strings.Repeat(`"SECRET",`, 300), truncated: true, want: `upstream answered 422`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := upstreamErrorText(http.StatusUnprocessableEntity, []byte(c.body), c.truncated); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Do itself tells a body cut at the size limit: a JSON error larger than the
// limit, which no longer parses once cut, is never echoed raw; a plain-text
// one is echoed cut to the limit, as before.
func TestDo_ALargeUpstreamErrorBody(t *testing.T) {
	bigJSON := `{"details":{"uid":"SECRET"},"pad":"` + strings.Repeat("x", 3*maxUnstructuredResponseTextBytes) + `"}`
	bigText := strings.Repeat("y", 3*maxUnstructuredResponseTextBytes)
	for _, c := range []struct {
		name, body, want string
	}{
		{name: "json", body: bigJSON, want: "upstream answered 422"},
		{name: "text", body: bigText, want: bigText[:maxUnstructuredResponseTextBytes]},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			st := Do(context.Background(), RequestOptions{
				RequestInfo: RequestInfo{Path: "/x", Verb: ptr.To(http.MethodGet)},
				Endpoint:    &endpoints.Endpoint{ServerURL: srv.URL},
			})
			if st == nil || st.Message != c.want || st.Code != http.StatusUnprocessableEntity {
				got := "<nil>"
				if st != nil {
					got = fmt.Sprintf("code %d, message of %d bytes: %.60q", st.Code, len(st.Message), st.Message)
				}
				t.Errorf("got %s; want code 422 and a message of %d bytes", got, len(c.want))
			}
		})
	}
}
