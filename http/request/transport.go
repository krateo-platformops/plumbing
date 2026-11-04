package request

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
)

func tlsConfigFor(ep *endpoints.Endpoint) (http.RoundTripper, error) {
	res := defaultTransport()

	if ep.Insecure {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
		}
		res.TLSClientConfig = tlsConfig
	}

	if ep.ProxyURL != "" {
		u, err := parseProxyURL(ep.ProxyURL)
		if err != nil {
			return nil, err
		}

		res.Proxy = http.ProxyURL(u)
	}

	if !ep.HasCertAuth() {
		return res, nil
	}

	certData, err := base64.StdEncoding.DecodeString(ep.ClientCertificateData)
	if err != nil {
		return nil, fmt.Errorf("unable to decode client certificate data")
	}

	keyData, err := base64.StdEncoding.DecodeString(ep.ClientKeyData)
	if err != nil {
		return nil, fmt.Errorf("unable to decode client key data")
	}

	cert, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		return res, err
	}

	caCertPool := x509.NewCertPool()

	if len(ep.CertificateAuthorityData) > 0 {
		caData, err := base64.StdEncoding.DecodeString(ep.CertificateAuthorityData)
		if err != nil {
			return nil, fmt.Errorf("unable to decode certificate authority data")
		}

		caCertPool.AppendCertsFromPEM(caData)
	}

	tlsConfig := &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
	}

	res.TLSClientConfig = tlsConfig
	return res, nil
}

func defaultTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func parseProxyURL(proxyURL string) (*url.URL, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("could not parse: %v", proxyURL)
	}

	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return nil, fmt.Errorf("unsupported scheme %q, must be http, https, or socks5", u.Scheme)
	}
	return u, nil
}

type traceIdRoundTripper struct {
	delegatedRoundTripper http.RoundTripper
}

func (rt *traceIdRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	traceId := req.Header.Get(xcontext.LabelKrateoTraceId)
	if len(traceId) == 0 {
		traceId = xcontext.TraceId(req.Context(), true)
	}
	req.Header.Set(xcontext.LabelKrateoTraceId, traceId)

	return rt.delegatedRoundTripper.RoundTrip(req)
}

type debuggingRoundTripper struct {
	delegatedRoundTripper http.RoundTripper
}

func (rt *debuggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	log := xcontext.Logger(req.Context())

	b, err := httputil.DumpRequestOut(req, true)
	if err != nil {
		return nil, err
	}
	log.Debug("request details", "wire", string(b))

	resp, err := rt.delegatedRoundTripper.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	b, err = httputil.DumpResponse(resp, req.URL.Query().Get("watch") != "true")
	if err != nil {
		return nil, err
	}
	log.Debug("response details", "wire", string(b))
	//fmt.Fprintf(os.Stderr, "\n\n%s\n\n", b)

	return resp, err
}

type basicAuthRoundTripper struct {
	username       string
	password       string `datapolicy:"password"`
	originHost     string
	passOnRedirect bool
	rt             http.RoundTripper
}

func (rt *basicAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(req.Header.Get("Authorization")) != 0 {
		return rt.rt.RoundTrip(req)
	}
	if !sameHost(req, rt.originHost, rt.passOnRedirect) {
		return rt.rt.RoundTrip(req)
	}
	req = cloneRequest(req)
	req.SetBasicAuth(rt.username, rt.password)
	return rt.rt.RoundTrip(req)
}

type bearerAuthRoundTripper struct {
	bearer         string
	originHost     string
	passOnRedirect bool
	rt             http.RoundTripper
}

func (rt *bearerAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(req.Header.Get("Authorization")) != 0 {
		return rt.rt.RoundTrip(req)
	}
	if !sameHost(req, rt.originHost, rt.passOnRedirect) {
		return rt.rt.RoundTrip(req)
	}

	req = cloneRequest(req)
	token := rt.bearer

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	return rt.rt.RoundTrip(req)
}

type awsAuthRoundTripper struct {
	headerPayload  string
	originHost     string
	passOnRedirect bool
	rt             http.RoundTripper
}

func (rt *awsAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(req.Header.Get("Authorization")) != 0 {
		return rt.rt.RoundTrip(req)
	}
	if !sameHost(req, rt.originHost, rt.passOnRedirect) {
		return rt.rt.RoundTrip(req)
	}

	req = cloneRequest(req)
	token := rt.headerPayload

	req.Header.Set("Authorization", token)
	return rt.rt.RoundTrip(req)
}

// sameHost reports whether req is going to the host the credential was configured for
// (core-provider#307).
//
// WHY THE ROUND-TRIPPER AND NOT CheckRedirect. net/http strips Authorization on a redirect, and
// these round-trippers then put it back: their guard was "if the header is ABSENT, add it", and
// after the strip it is absent. So the stdlib's protection never engaged. A CheckRedirect alone
// cannot fix that, because the round-tripper runs AFTER it — the guard has to be here, where the
// credential is attached, so that no later layer can undo it.
//
// EXACT host:port, and no subdomain allowance, deliberately. The stdlib permits a hop to a
// SUBDOMAIN of the origin and ignores the port entirely — its own comment says so
// (client.go:992-1009, "Permit sending auth/cookie headers from foo.com to sub.foo.com"), and
// idnaASCIIFromURL compares url.Hostname() (transport.go:2947). Both were measured on a live
// harness by the oasgen-provider session and then confirmed in the Go source. So
// api.example.com -> evil.api.example.com carries the credential under the stdlib and must not
// here.
//
// A string-suffix or Contains matcher would be WORSE than the stdlib rather than better: it admits
// api.example.com.attacker.net, a name an attacker can register. Equality is the only comparison
// that gets both axes right, which is why this is == and not a helper with an allowlist.
//
// An EMPTY originHost refuses. That is the destructive-default rule on a security path: a value we
// could not determine must not become a value we act on. The cost is a 401 an operator can see.
func sameHost(req *http.Request, originHost string, passOnRedirect bool) bool {
	// The OPT-OUT, and it is checked before the anchor on purpose: an operator who set it has
	// declared that this endpoint's redirects are trusted, and that declaration should not be
	// overridden by an anchor we could not parse. Refusing an explicitly-permitted endpoint is the
	// failure mode that looks like a pass when you only test the attack.
	if passOnRedirect {
		return true
	}
	if originHost == "" || req.URL == nil {
		return false
	}
	return req.URL.Host == originHost
}
