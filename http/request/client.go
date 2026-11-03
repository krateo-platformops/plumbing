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

	// THE HOST THE OPERATOR CONFIGURED is the trust anchor for every credential below
	// (core-provider#307). Captured once, here, because this is the only place that still knows it:
	// by the time a RoundTrip runs on a redirected request, the original host is gone.
	//
	// An unparseable or empty ServerURL yields "", and the round-trippers refuse to attach on an
	// empty anchor — FAIL CLOSED. The cost is a 401 the operator can see; the alternative is a
	// credential sent to a host nobody chose, which is silent.
	originHost := originHostOf(ep.ServerURL)

	switch {
	case countTrue > 1:
		return nil, fmt.Errorf("only one of username/password, bearer token and AWS must be set")

	case ep.HasTokenAuth():
		rt = &bearerAuthRoundTripper{
			bearer:     ep.Token,
			originHost: originHost,
			rt:         rt,
		}

	case ep.HasBasicAuth():
		rt = &basicAuthRoundTripper{
			username:   ep.Username,
			password:   ep.Password,
			originHost: originHost,
			rt:         rt,
		}

	case ep.HasAwsAuth():
		if ex == nil {
			return nil, fmt.Errorf("extra request options required for AWS authentication")
		} else {
			rt = &awsAuthRoundTripper{
				headerPayload: ComputeAwsSignature(ep, ex),
				originHost:    originHost,
				rt:            rt,
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
	// This strips on ANY host change, exact on host:port, which is what the chart getter already
	// does (plumbing helm/getter/getter.go:106-114) and is stronger than the stdlib on both axes.
	//
	// It is NOT sufficient alone: the auth round-trippers run AFTER CheckRedirect and would re-add
	// the header. The host-aware guard in each of them is the real fix; this covers an Authorization
	// a CALLER set directly via opts.Headers, which no round-tripper knows about.
	return &http.Client{
		Transport: rt,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}, nil
}

// originHostOf returns the host:port of a configured server URL, or "" when it cannot be parsed.
//
// "" means "no anchor", and every credential round-tripper treats that as refuse-to-attach. That is
// the destructive-default rule applied to a security path: a value we could not determine must not
// become a value we act on.
func originHostOf(serverURL string) string {
	if serverURL == "" {
		return ""
	}
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
