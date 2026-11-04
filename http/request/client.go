package request

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/krateo-platformops/plumbing/endpoints"
)

func HTTPClientForEndpoint(ep *endpoints.Endpoint, ex *RequestInfo) (*http.Client, error) {
	rt, err := tlsConfigFor(ep)
	if err != nil {
		return &http.Client{
			Transport: &traceIdRoundTripper{defaultTransport()},
		}, err
	}
	rt = &traceIdRoundTripper{rt}

	if ep.Debug {
		rt = &debuggingRoundTripper{
			delegatedRoundTripper: rt,
		}
	}

	// Set authentication wrapper
	// Only one can be active at a time
	countTrue := 0
	if ep.HasBasicAuth() {
		countTrue += 1
	}
	if ep.HasTokenAuth() {
		countTrue += 1
	}
	if ep.HasAwsAuth() {
		countTrue += 1
	}

	// THE ORIGIN THE OPERATOR CONFIGURED is the trust anchor for every credential below
	// (core-provider#307): the scheme AND the host:port. Captured once, here, because this is the
	// only place that still knows it: by the time a RoundTrip runs on a redirected request, the
	// original origin is gone.
	//
	// SCHEME is part of the anchor because the first version of this fix compared the host alone,
	// and url.Host carries no scheme — so an https endpoint that redirected to plaintext http on
	// the SAME host was "the same host" and the credential went out in the clear. See origin.permits.
	//
	// An unparseable or empty ServerURL yields the zero origin, and the round-trippers refuse to
	// attach on an unknown anchor — FAIL CLOSED. The cost is a 401 the operator can see; the
	// alternative is a credential sent to a host nobody chose, which is silent.
	anchor := originOf(ep.ServerURL)
	passOnRedirect := ep.PassCredentialsOnRedirect

	switch {
	case countTrue > 1:
		return nil, fmt.Errorf("only one of username/password, bearer token and AWS must be set")

	case ep.HasTokenAuth():
		rt = &bearerAuthRoundTripper{
			bearer:         ep.Token,
			origin:         anchor,
			passOnRedirect: passOnRedirect,
			rt:             rt,
		}

	case ep.HasBasicAuth():
		rt = &basicAuthRoundTripper{
			username:       ep.Username,
			password:       ep.Password,
			origin:         anchor,
			passOnRedirect: passOnRedirect,
			rt:             rt,
		}

	case ep.HasAwsAuth():
		if ex == nil {
			return nil, fmt.Errorf("extra request options required for AWS authentication")
		} else {
			rt = &awsAuthRoundTripper{
				headerPayload:  ComputeAwsSignature(ep, ex),
				origin:         anchor,
				passOnRedirect: passOnRedirect,
				rt:             rt,
			}
		}
	}

	// DEFENCE IN DEPTH, and deliberately not the primary guard.
	//
	// net/http strips Authorization on a redirect only when the destination is NOT the origin or a
	// subdomain of it, and it compares url.Hostname() so a PORT change does not count as a change
	// at all (client.go:992 shouldCopyHeaderOnRedirect -> isDomainOrSubdomain; transport.go:2947
	// idnaASCIIFromURL). Its own comment says the subdomain allowance is intentional. So the stdlib
	// permits api.example.com -> evil.api.example.com, measured by the oasgen-provider session on a
	// live harness and then confirmed in the Go source.
	//
	// This strips on any ORIGIN change — exact on host:port, and on a scheme downgrade — which is
	// stronger than the stdlib on every axis it gets wrong.
	//
	// It is compared against via[0], the FIRST request of the chain, rather than against the
	// configured anchor: this layer exists for an Authorization a CALLER set directly via
	// opts.Headers, and such a caller's own first request is the origin it chose. The round-trippers
	// below use the configured anchor, which is the right reference for a credential THEY attach.
	//
	// It is NOT sufficient alone: the auth round-trippers run AFTER CheckRedirect and would re-add
	// the header. The origin-aware guard in each of them is the real fix; this covers the header a
	// caller set, which no round-tripper knows about.
	return &http.Client{
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if passOnRedirect || len(via) == 0 {
				return nil
			}
			if !originOfURL(via[0].URL).permits(originOfURL(req.URL)) {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}, nil
}

// originOf returns the anchor of a configured server URL, or the zero origin when it cannot be
// determined.
//
// The zero origin means "no anchor", and every credential round-tripper treats that as
// refuse-to-attach. That is the destructive-default rule applied to a security path: a value we
// could not determine must not become a value we act on.
func originOf(serverURL string) origin {
	if serverURL == "" {
		return origin{}
	}
	u, err := url.Parse(serverURL)
	if err != nil {
		return origin{}
	}
	return originOfURL(u)
}
