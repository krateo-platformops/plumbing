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
// THE SCHEME AXIS was added after the fact, and the reason is worth keeping: the first version of
// this guard compared url.Host alone, which carries no scheme. Measured, with a control:
//
//	https://api.example.com -> http://api.example.com   same Host? true
//	https://api.example.com -> https://evil.example.net  same Host? false
//
// So a TLS endpoint that redirected to plaintext on its OWN host passed the guard and the bearer
// token went out in the clear. The snowplow session compared scheme+host+port at their egress and
// said so in passing; that remark is what prompted the measurement. Every row below therefore
// varies the whole destination URL rather than just the host — a table that fixes the scheme cannot
// fail when the scheme is the thing being ignored, which is exactly how the first version shipped.
func TestSameOrigin_Axes(t *testing.T) {
	anchor := originOf("https://api.example.com")
	if !anchor.known() {
		t.Fatal("premise: the fixture anchor must parse, or every row below refuses for the " +
			"wrong reason and the negative rows pass vacuously")
	}

	for _, tc := range []struct {
		name    string
		reqURL  string
		want    bool
		because string
	}{
		{
			name: "exact match", reqURL: "https://api.example.com/x", want: true,
			because: "the configured origin must work, or the guard refuses everything and the " +
				"negative rows below pass for the wrong reason",
		},
		{
			name: "SUBDOMAIN of the origin", reqURL: "https://evil.api.example.com/x", want: false,
			because: "net/http PERMITS this hop — client.go:992 isDomainOrSubdomain, and its own " +
				"comment says the allowance is intentional. We must not inherit it",
		},
		{
			name: "attacker-registered PARENT", reqURL: "https://api.example.com.attacker.net/x", want: false,
			because: "a HasSuffix or Contains matcher admits this, and attacker.net is registrable. " +
				"This is the row a reimplementation fails, and it is why the comparison is == " +
				"rather than a suffix test",
		},
		{
			name: "same name, DIFFERENT port", reqURL: "https://api.example.com:8443/x", want: false,
			because: "net/http compares url.Hostname() (transport.go:2947), so a port change is " +
				"invisible to it. Comparing url.Host makes it visible here",
		},
		{
			name: "unrelated host", reqURL: "https://attacker.net/x", want: false,
			because: "the baseline case, and the one the stdlib does catch",
		},
		{
			name: "prefix of the origin", reqURL: "https://example.com/x", want: false,
			because: "a HasPrefix matcher would reject this and admit the parent row above; both " +
				"directions have to be wrong for the right reason",
		},
		{
			name: "TLS DOWNGRADE to the same host", reqURL: "http://api.example.com/x", want: false,
			because: "THE ROW THE FIRST VERSION FAILED. url.Host is identical for both, so a " +
				"host-only comparison sends the token in cleartext to the very host the operator " +
				"configured for TLS, and Go follows such a redirect by default",
		},
		{
			name: "TLS downgrade with the default port spelled out", reqURL: "http://api.example.com:80/x", want: false,
			because: "default-port elision must not launder a downgrade: :80 normalises away, so " +
				"the host matches and only the scheme check can refuse this",
		},
		{
			name: "explicit default port on the SAME scheme", reqURL: "https://api.example.com:443/x", want: true,
			because: "https://h and https://h:443 are one endpoint. Refusing this would be a FALSE " +
				"refusal, and a guard that over-refuses gets switched off",
		},
		{
			name: "UPPERCASE host", reqURL: "https://API.Example.COM/x", want: true,
			because: "DNS is case-insensitive, so this is the same host. Another false-refusal row: " +
				"the normalisation is applied to both sides, so it cannot admit a host that exact " +
				"comparison would reject",
		},
		{
			name: "a non-HTTP scheme on the same host", reqURL: "ftp://api.example.com/x", want: false,
			because: "only the http->https improvement is permitted; any other scheme change is a " +
				"channel we never evaluated",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.reqURL)
			if err != nil {
				t.Fatalf("fixture URL: %v", err)
			}
			req := &http.Request{URL: u, Header: http.Header{}}

			if got := sameOrigin(req, anchor, false); got != tc.want {
				t.Errorf("sameOrigin(%q, anchor=%s) = %v, want %v: %s",
					tc.reqURL, anchor, got, tc.want, tc.because)
			}
		})
	}

	// THE UPGRADE, as its own case because its anchor differs: configured http, redirected to https
	// on the same host. This is the ordinary HSTS hop and it must be PERMITTED — the destination is
	// the same host over a strictly better channel, and refusing it would break a real deployment
	// for no security gain. It is also the row that proves the scheme rule is DIRECTIONAL rather
	// than plain equality, which a `o.scheme == dst.scheme` mutant would pass the downgrade rows on.
	t.Run("http -> https upgrade on the same host is permitted", func(t *testing.T) {
		plain := originOf("http://api.example.com")
		if !plain.known() {
			t.Fatal("premise: the http anchor must parse")
		}
		u, _ := url.Parse("https://api.example.com/x")
		if !sameOrigin(&http.Request{URL: u, Header: http.Header{}}, plain, false) {
			t.Error("an http -> https hop to the same host must be permitted: refusing it is a " +
				"false refusal on the commonest redirect there is, and the channel improves")
		}
		// And the reverse from the same pair, so the direction is asserted rather than assumed.
		d, _ := url.Parse("http://api.example.com/x")
		tls := originOf("https://api.example.com")
		if sameOrigin(&http.Request{URL: d, Header: http.Header{}}, tls, false) {
			t.Error("https -> http must still refuse; if both directions pass, the scheme check " +
				"has become unconditional")
		}
	})

	// No anchor refuses, whatever the request says.
	u, _ := url.Parse("https://api.example.com/x")
	if sameOrigin(&http.Request{URL: u, Header: http.Header{}}, origin{}, false) {
		t.Error("an unknown anchor must refuse: a value we could not determine must not become " +
			"a value we act on")
	}
	if sameOrigin(&http.Request{Header: http.Header{}}, anchor, false) {
		t.Error("a nil URL must refuse rather than panic")
	}

	// An anchor we could not parse must be UNKNOWN, not a zero-host origin that accidentally
	// matches a request with no host.
	for _, bad := range []string{"", "not a url", "https://", "/only/a/path"} {
		if originOf(bad).known() {
			t.Errorf("originOf(%q) must not yield an anchor", bad)
		}
	}
}

// THE OPT-OUT, and it needs its own rows because a security escape hatch that silently does
// nothing is worse than not having one (core-provider#307).
//
// The strict default has a legitimate casualty: an endpoint configured without a port that
// redirects to one — an on-prem GHES, a corporate proxy — is a real deployment, and helm's own
// chart getter has carried PassCredentialsAll for exactly that since long before this. Measured on
// krateo-057 by the installer session: 0 of 3 external targets issue any 3xx, so nothing there
// needs it. The field is for the deployment that does.
func TestPassCredentialsOnRedirect(t *testing.T) {
	t.Run("OFF by default: the credential does not follow", func(t *testing.T) {
		origin, sawAuth := redirectPair(t, func(destURL string) string { return destURL })
		ep := &endpoints.Endpoint{ServerURL: origin, Token: "SECRET-TOKEN"}
		cli, err := HTTPClientForEndpoint(ep, nil)
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		resp, err := cli.Get(origin)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if *sawAuth != "" {
			t.Errorf("default must refuse, destination saw %q", *sawAuth)
		}
	})

	t.Run("ON: the credential follows, because the operator said so", func(t *testing.T) {
		origin, sawAuth := redirectPair(t, func(destURL string) string { return destURL })
		ep := &endpoints.Endpoint{
			ServerURL:                 origin,
			Token:                     "SECRET-TOKEN",
			PassCredentialsOnRedirect: true,
		}
		cli, err := HTTPClientForEndpoint(ep, nil)
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		resp, err := cli.Get(origin)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		// THIS is the arm that matters. An escape hatch asserted only by "the default refuses"
		// could be implemented as a no-op and look correct — the operator sets it, nothing
		// changes, and the RESTAction still 401s with no indication why.
		if *sawAuth != "Bearer SECRET-TOKEN" {
			t.Errorf("with PassCredentialsOnRedirect the destination must see the credential, "+
				"saw %q. An opt-out that does nothing is worse than none: the operator believes "+
				"they have re-enabled it", *sawAuth)
		}
	})

	// And at the unit level: the opt-out wins even when the anchor is unusable. An operator who
	// declared this endpoint's redirects trusted should not be overridden by a ServerURL we could
	// not parse — refusing an explicitly-permitted endpoint is the failure that looks like a pass.
	t.Run("ON overrides an unknown anchor", func(t *testing.T) {
		u, _ := url.Parse("https://anywhere.example.com/x")
		req := &http.Request{URL: u, Header: http.Header{}}
		if !sameOrigin(req, origin{}, true) {
			t.Error("the opt-out must win over an unparseable anchor")
		}
		if sameOrigin(req, origin{}, false) {
			t.Error("and without it, an unknown anchor must still refuse")
		}
	})

	// AND IT MUST ALSO COVER THE DOWNGRADE, because that is the whole point of an opt-out: an
	// operator who declared this endpoint's redirects trusted has accepted its hops, and a guard
	// that honoured the opt-out on the host axis but silently kept refusing on the scheme axis
	// would be the "opt-out that does nothing" shape in a narrower disguise.
	t.Run("ON also permits a downgrade the operator accepted", func(t *testing.T) {
		d, _ := url.Parse("http://api.example.com/x")
		req := &http.Request{URL: d, Header: http.Header{}}
		anchor := originOf("https://api.example.com")
		if !sameOrigin(req, anchor, true) {
			t.Error("with the opt-out set, the scheme check must not keep refusing: a partial " +
				"escape hatch leaves the operator with a 401 and no explanation")
		}
		if sameOrigin(req, anchor, false) {
			t.Error("CONTROL: without it the same hop must refuse, or the row above proves nothing")
		}
	})
}
