//go:build js && wasm

package dialguard

import (
	"net/http"
	"strings"
	"syscall/js"
)

// The request headers net/http's js/wasm round-trip reads as fetch's options, and removes before
// sending (net/http/roundtrip_js.go).
const (
	jsFetchRedirect    = "js.fetch:redirect"
	jsFetchCredentials = "js.fetch:credentials"
)

// usesFetch is net/http's own choice on js/wasm, mirrored (roundtrip_js.go, jsFetchMissing and
// jsFetchDisabled, unchanged through go1.26): fetch is used when the global fetch exists and the
// program isn't running under Node.js. Under Node, net/http uses Go's in-process network, follows
// redirects itself, CheckRedirect included, and rejects a js.fetch: header as an invalid field
// name, so none is set there. A Transport with a dial function never uses fetch either; Transport
// has none on js/wasm.
var usesFetch = !js.Global().Get("fetch").IsUndefined() &&
	!(js.Global().Get("process").Type() == js.TypeObject &&
		strings.HasPrefix(js.Global().Get("process").Get("argv0").String(), "node"))

// fetchPolicy makes every request through rt use, when fetch is the path:
//
//   - redirect mode "error": a redirect fails the request. Not "manual": fetch then answers an
//     opaque response with no status or Location, so Go could not apply its own policy either.
//   - credentials mode "omit": the browser sends no cookies, HTTP authentication or client
//     certificate of its own. fetch's default, "same-origin", sends the page origin's cookies to
//     a same-origin URL, so a plumbing client fetching from the frame's own origin would carry
//     whatever session cookies that origin set, credentials no plumbing caller chose. Plumbing's
//     clients authenticate with the Authorization header they set themselves, which "omit" leaves
//     alone.
//
// Both are set on every request, overriding the caller's, so no plumbing client can be made to
// follow a hop Go doesn't see or to send credentials it didn't set.
func fetchPolicy(rt http.RoundTripper) http.RoundTripper {
	if !usesFetch {
		return rt
	}
	return fetchOptions{rt: rt}
}

type fetchOptions struct{ rt http.RoundTripper }

func (f fetchOptions) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context()) // a RoundTripper must not modify the caller's request
	r.Header.Set(jsFetchRedirect, "error")
	r.Header.Set(jsFetchCredentials, "omit")
	return f.rt.RoundTrip(r)
}
