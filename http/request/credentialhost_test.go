package request

// A credential must not follow a redirect away from the host it was configured for
// (core-provider#307).
//
// THE DEFECT: net/http strips Authorization on a cross-host redirect, and these round-trippers then
// put it back — their guard was "if the header is ABSENT, add it", and after the strip it is
// absent. The stdlib's protection never engaged, and no CheckRedirect existed in this package to
// compensate.
//
// AND THE STDLIB IS WEAKER THAN IT LOOKS, which is why equality is the comparison rather than
// "not a cross-host hop". Confirmed in the Go source, not inferred:
//
//	client.go:992-1009   shouldCopyHeaderOnRedirect -> isDomainOrSubdomain, and its own comment
//	                     says "Permit sending auth/cookie headers from foo.com to sub.foo.com"
//	transport.go:2947    idnaASCIIFromURL compares url.Hostname() — THE PORT IS EXCLUDED
//
// So the stdlib permits a SUBDOMAIN hop and a PORT change. The oasgen-provider session measured
// both on a live harness with negative controls before I read the source.
//
// The fixture is a real redirect through two httptest servers, so the stdlib's own behaviour is in
// the loop rather than mocked — a test that stubbed the redirect would be asserting my model of
// net/http, which is the mistake that let the defect exist.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/krateo-platformops/plumbing/endpoints"
)

// relayTo returns a server that 302s to target and records what the FINAL destination saw.
func redirectPair(t *testing.T, rewriteHost func(originURL string) string) (origin string, sawAuth *string) {
	t.Helper()

	seen := ""
	sawAuth = &seen

	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(dest.Close)

	from := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, rewriteHost(dest.URL), http.StatusFound)
	}))
	t.Cleanup(from.Close)

	return from.URL, sawAuth
}

func TestCredentialDoesNotFollowARedirectOffTheConfiguredHost(t *testing.T) {
	origin, sawAuth := redirectPair(t, func(destURL string) string { return destURL })

	ep := &endpoints.Endpoint{ServerURL: origin, Token: "SECRET-TOKEN"}
	cli, err := HTTPClientForEndpoint(ep, nil)
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	// CALIBRATION FIRST: the credential must reach the ORIGIN, or this test proves nothing — a
	// round-tripper that attached nothing at all would pass the assertion below.
	reached := ""
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = r.Header.Get("Authorization")
	}))
	defer probe.Close()
	probeEP := &endpoints.Endpoint{ServerURL: probe.URL, Token: "SECRET-TOKEN"}
	probeCli, err := HTTPClientForEndpoint(probeEP, nil)
	if err != nil {
		t.Fatalf("building the probe client: %v", err)
	}
	if _, err := probeCli.Get(probe.URL); err != nil {
		t.Fatalf("probe request: %v", err)
	}
	if reached != "Bearer SECRET-TOKEN" {
		t.Fatalf("CONTROL FAILED: the configured host saw %q, want the bearer token. The guard is "+
			"refusing everything, so the assertion below would pass for the wrong reason", reached)
	}

	// THE ASSERTION: a hop to a different host must not carry it.
	resp, err := cli.Get(origin)
	if err != nil {
		t.Fatalf("request through the redirect: %v", err)
	}
	resp.Body.Close()

	if *sawAuth != "" {
		t.Errorf("the redirect destination received %q. net/http strips Authorization on a "+
			"cross-host hop and the round-tripper used to re-add it, because its guard was "+
			"\"if absent, add\" — and after the strip it IS absent (#307)", *sawAuth)
	}
}

// An empty or unparseable ServerURL must refuse to attach, not attach anyway.
//
// The destructive-default rule on a security path: a value that could not be determined must not
// become a value we act on. The cost is a 401 an operator can see; the alternative is a credential
// sent to a host nobody chose.
func TestNoAnchorMeansNoCredential(t *testing.T) {
	for _, tc := range []struct{ name, serverURL string }{
		{"empty ServerURL", ""},
		{"unparseable ServerURL", "ht tp://not a url"},
		{"scheme only, no host", "https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := "unset"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Get("Authorization")
			}))
			defer srv.Close()

			ep := &endpoints.Endpoint{ServerURL: tc.serverURL, Token: "SECRET-TOKEN"}
			cli, err := HTTPClientForEndpoint(ep, nil)
			if err != nil {
				t.Fatalf("building the client: %v", err)
			}
			if _, err := cli.Get(srv.URL); err != nil {
				t.Fatalf("request: %v", err)
			}

			if seen != "" {
				t.Errorf("with no usable anchor the destination saw %q; it must see nothing. "+
					"Attaching on an undetermined anchor is how a credential reaches a host "+
					"nobody configured", seen)
			}
		})
	}
}

// The comparison axes, as a unit over sameHost — because the end-to-end test above CANNOT reach
// them and a mutant proved it.
//
// Replacing `==` with `strings.HasSuffix` in sameHost COMPILES and leaves
// TestCredentialDoesNotFollowARedirectOffTheConfiguredHost PASSING, because httptest servers are
// 127.0.0.1 on different ports and a suffix match between two ports is false anyway. So that test
// covers the cross-host axis and nothing else.
//
// The axis it misses is the one the oasgen-provider session named as most likely to be failed by a
// reimplementation, and they were right: api.example.com.attacker.net against a configured
// api.example.com. A suffix or Contains matcher ADMITS it, and it is a name an attacker can
// register. Their own suite covers the leading-label axis and never covered this one.
func TestSameHost_Axes(t *testing.T) {
	const origin = "api.example.com"

	for _, tc := range []struct {
		name    string
		reqHost string
		want    bool
		because string
	}{
		{
			name: "exact match", reqHost: "api.example.com", want: true,
			because: "the configured host must work, or the guard refuses everything and the " +
				"negative rows below pass for the wrong reason",
		},
		{
			name: "SUBDOMAIN of the origin", reqHost: "evil.api.example.com", want: false,
			because: "net/http PERMITS this hop — client.go:992 isDomainOrSubdomain, and its own " +
				"comment says the allowance is intentional. We must not inherit it",
		},
		{
			name: "attacker-registered PARENT", reqHost: "api.example.com.attacker.net", want: false,
			because: "a HasSuffix or Contains matcher admits this, and attacker.net is registrable. " +
				"This is the row a reimplementation fails, and it is why the comparison is == " +
				"rather than a suffix test",
		},
		{
			name: "same name, DIFFERENT port", reqHost: "api.example.com:8443", want: false,
			because: "net/http compares url.Hostname() (transport.go:2947), so a port change is " +
				"invisible to it. Comparing url.Host makes it visible here",
		},
		{
			name: "unrelated host", reqHost: "attacker.net", want: false,
			because: "the baseline case, and the one the stdlib does catch",
		},
		{
			name: "prefix of the origin", reqHost: "example.com", want: false,
			because: "a HasPrefix matcher would reject this and admit the parent row above; both " +
				"directions have to be wrong for the right reason",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse("https://" + tc.reqHost + "/x")
			if err != nil {
				t.Fatalf("fixture URL: %v", err)
			}
			req := &http.Request{URL: u, Header: http.Header{}}

			if got := sameHost(req, origin); got != tc.want {
				t.Errorf("sameHost(%q, %q) = %v, want %v: %s",
					tc.reqHost, origin, got, tc.want, tc.because)
			}
		})
	}

	// No anchor refuses, whatever the request says.
	u, _ := url.Parse("https://api.example.com/x")
	if sameHost(&http.Request{URL: u, Header: http.Header{}}, "") {
		t.Error("an empty origin host must refuse: a value we could not determine must not become " +
			"a value we act on")
	}
	if sameHost(&http.Request{Header: http.Header{}}, origin) {
		t.Error("a nil URL must refuse rather than panic")
	}
}
