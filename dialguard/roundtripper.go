package dialguard

import "net/http"

// RoundTripper returns rt the way every client built on Transport must send through it.
//
// On a server build it is rt itself. On js/wasm (redirect_js.go) it refuses every redirect,
// because there the browser's fetch follows a redirect on its own and Go never sees the hop: an
// http.Client's CheckRedirect, and the origin checks plumbing's credential round-trippers make on
// a hop (http/request, helm/getter), would silently not apply, and a JWKS fetch could be bounced
// to keys served from anywhere. A refused redirect is an error the caller sees, never a request
// sent somewhere unchecked.
//
// Client wraps its Transport with it, as do plumbing's own client constructors. A caller that
// builds an http.Client on Transport directly must wrap it too.
func RoundTripper(rt http.RoundTripper) http.RoundTripper {
	return refuseRedirects(rt)
}
