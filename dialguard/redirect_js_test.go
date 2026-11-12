//go:build js && wasm

package dialguard

import (
	"net/http"
	"os"
	"testing"
)

type recordingRT struct{ got *http.Request }

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

// Where fetch is the path, RoundTripper sets fetch's redirect mode to "error" and its credentials
// mode to "omit" on every request, overriding the caller's, and leaves the caller's request as it
// was. Where it isn't (Node), it is the identity: Go follows redirects itself there, with
// CheckRedirect.
func TestRoundTripperSetsTheFetchPolicyOnJS(t *testing.T) {
	// Run by TestPlumbingClientsKeepTheFetchPolicyOnJS, the mode net/http uses is known: usesFetch
	// must match it, in both directions.
	if mode := os.Getenv("PLUMBING_JS_MODE"); mode != "" && usesFetch != (mode == "fetch") {
		t.Fatalf("usesFetch = %v under %s: it no longer mirrors net/http's choice", usesFetch, mode)
	}
	rec := &recordingRT{}
	if !usesFetch {
		if got := RoundTripper(rec); got != http.RoundTripper(rec) {
			t.Errorf("RoundTripper wrapped the transport where fetch isn't used: %T", got)
		}
		return
	}
	req, err := http.NewRequest(http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(jsFetchRedirect, "follow")
	req.Header.Set(jsFetchCredentials, "include")
	req.Header.Set("Authorization", "Bearer caller-set")
	if _, err := RoundTripper(rec).RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got := rec.got.Header.Get(jsFetchRedirect); got != "error" {
		t.Errorf("redirect mode %q, want error", got)
	}
	if got := rec.got.Header.Get(jsFetchCredentials); got != "omit" {
		t.Errorf("credentials mode %q, want omit", got)
	}
	if got := rec.got.Header.Get("Authorization"); got != "Bearer caller-set" {
		t.Errorf("the caller's own Authorization was changed: %q", got)
	}
	if got := req.Header.Get(jsFetchRedirect); got != "follow" {
		t.Errorf("the caller's request was modified: %q", got)
	}
	if got := req.Header.Get(jsFetchCredentials); got != "include" {
		t.Errorf("the caller's request was modified: %q", got)
	}
}

// Run by TestPlumbingClientsKeepTheFetchPolicyOnJS against a redirecting server: Client must fail
// the request, not follow it.
func TestJSRefusesRedirects(t *testing.T) {
	url := os.Getenv("PLUMBING_JS_REDIRECT_URL")
	if url == "" {
		t.Skip("run by TestPlumbingClientsKeepTheFetchPolicyOnJS")
	}
	resp, err := Client(0).Get(url)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("a redirect was followed: status %d", resp.StatusCode)
	}
	t.Logf("refused: %v", err)
}
