package request

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
)

// An endpoint with Debug set logs each request as metadata only (#160). The debug round tripper
// sits under the credential round trippers, so the request it sees carries the endpoint's
// credential; a wire dump would put it in the log, along with the query, the request and response
// bodies, and Set-Cookie. Each case proves the credential reached the server and that the debug
// line was written, then requires the log to hold none of those.
func TestHTTPClientForEndpoint_DebugLogHoldsNoCredential(t *testing.T) {
	const (
		token        = "bearer-token-for-the-debug-test"
		username     = "user-for-the-debug-test"
		password     = "password-for-the-debug-test"
		queryValue   = "query-value-for-the-debug-test"
		cookieValue  = "cookie-value-for-the-debug-test"
		reqBodyValue = "request-body-value-for-the-debug-test"
		resBodyValue = "response-body-value-for-the-debug-test"
	)
	cases := []struct {
		name      string
		ep        endpoints.Endpoint
		sawCred   func(*http.Request) bool
		forbidden map[string]string
	}{
		{
			name:      "bearer",
			ep:        endpoints.Endpoint{Token: token},
			sawCred:   func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+token },
			forbidden: map[string]string{"the token": token, "a bearer value": "Bearer"},
		},
		{
			name: "basic",
			ep:   endpoints.Endpoint{Username: username, Password: password},
			sawCred: func(r *http.Request) bool {
				u, p, ok := r.BasicAuth()
				return ok && u == username && p == password
			},
			forbidden: map[string]string{"the password": password, "a basic value": "Basic"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sawCred := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawCred = tc.sawCred(r)
				w.Header().Set("Set-Cookie", "session="+cookieValue)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"access_token":"` + resBodyValue + `"}`))
			}))
			defer srv.Close()

			ep := tc.ep
			ep.ServerURL = srv.URL
			ep.Debug = true
			hc, err := HTTPClientForEndpoint(&ep, nil)
			if err != nil {
				t.Fatalf("HTTPClientForEndpoint: an error of type %T", err)
			}

			var logs bytes.Buffer
			ctx := xcontext.BuildContext(context.Background(),
				xcontext.WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				srv.URL+"/token?code="+url.QueryEscape(queryValue), strings.NewReader(`{"v":"`+reqBodyValue+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatalf("POST: an error of type %T", err)
			}
			resp.Body.Close()
			if !sawCred {
				t.Fatal("the server did not see the endpoint's credential: the test could not see a leak")
			}

			out := logs.String()
			if out == "" {
				t.Fatal("nothing logged at DEBUG: the test could not see a leak")
			}
			forbidden := map[string]string{
				"an Authorization header": "Authorization", "the query": queryValue,
				"a Set-Cookie value": cookieValue, "the request body": reqBodyValue, "the response body": resBodyValue,
			}
			for k, v := range tc.forbidden {
				forbidden[k] = v
			}
			for what, s := range forbidden {
				if strings.Contains(out, s) {
					t.Errorf("the debug log holds %s", what)
				}
			}
			line := findLine(out, "http request")
			if line == nil {
				t.Fatalf("no request line at DEBUG (%d bytes logged): the test could not see a leak", len(out))
			}
			if line["method"] != "POST" || line["path"] != "/token" || line["status"] != float64(200) {
				t.Errorf("the request line's metadata: method=%v path=%v status=%v", line["method"], line["path"], line["status"])
			}
		})
	}
}

// A failed request logs the error's type, never its text: a transport error's text quotes the URL,
// query included.
func TestHTTPClientForEndpoint_DebugLogOfAFailureHoldsNoQuery(t *testing.T) {
	const queryValue = "query-value-for-the-failure-test"
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srvURL := srv.URL
	srv.Close() // nothing listens there now, so the dial fails

	ep := endpoints.Endpoint{ServerURL: srvURL, Token: "token-for-the-failure-test", Debug: true}
	hc, err := HTTPClientForEndpoint(&ep, nil)
	if err != nil {
		t.Fatalf("HTTPClientForEndpoint: an error of type %T", err)
	}
	var logs bytes.Buffer
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srvURL+"/x?code="+url.QueryEscape(queryValue), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("the request to a closed server succeeded: the test could not see the failure path")
	}
	if !strings.Contains(err.Error(), queryValue) {
		t.Fatal("the transport error does not quote the query: the test could not see a leak")
	}

	out := logs.String()
	if out == "" {
		t.Fatal("nothing logged at DEBUG: the test could not see a leak")
	}
	for what, s := range map[string]string{"the query": queryValue, "the token": "token-for-the-failure-test"} {
		if strings.Contains(out, s) {
			t.Errorf("the debug log holds %s", what)
		}
	}
	line := findLine(out, "http request: failed")
	if line == nil {
		t.Fatalf("no failure line at DEBUG (%d bytes logged): the test could not see a leak", len(out))
	}
	if et, _ := line["error_type"].(string); et == "" {
		t.Error("the failure line names no error type")
	}
}

func findLine(out, msg string) map[string]any {
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == msg {
			line = m
		}
	}
	return line
}
