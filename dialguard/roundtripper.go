package dialguard

import "net/http"

// RoundTripper returns rt the way every client built on Transport must send through it.
//
// On a server build it is rt itself. On js/wasm, where the browser's fetch sends the request
// (fetch_policy_js.go), it refuses every redirect and sends no browser credentials:
//
//   - fetch follows a redirect on its own and Go never sees the hop, so an http.Client's
//     CheckRedirect, and the origin checks plumbing's credential round-trippers make on a hop
//     (http/request, helm/getter), would silently not apply, and a JWKS fetch could be bounced to
//     keys served from anywhere. A refused redirect is an error the caller sees, never a request
//     sent somewhere unchecked.
//   - fetch's default sends the page origin's cookies to a same-origin URL. A plumbing client
//     sends only the credentials its caller set, in the Authorization header, never the page's.
//
// Client wraps its Transport with it, as do plumbing's own client constructors. A caller that
// builds an http.Client on Transport directly must wrap it too.
func RoundTripper(rt http.RoundTripper) http.RoundTripper {
	return fetchPolicy(rt)
}
