//go:build js && wasm

package dialguard

import (
	"net/http"
	"strings"
	"syscall/js"
)

// jsFetchRedirect is the request header net/http's js/wasm round-trip reads as fetch's redirect
// mode, and removes before sending (net/http/roundtrip_js.go).
const jsFetchRedirect = "js.fetch:redirect"

// usesFetch is net/http's own choice on js/wasm, mirrored (roundtrip_js.go, jsFetchMissing and
// jsFetchDisabled, unchanged through go1.26): fetch is used when the global fetch exists and the
// program isn't running under Node.js. Under Node, net/http uses Go's in-process network, follows
// redirects itself, CheckRedirect included, and rejects a js.fetch: header as an invalid field
// name, so none is set there. A Transport with a dial function never uses fetch either; Transport
// has none on js/wasm.
var usesFetch = !js.Global().Get("fetch").IsUndefined() &&
	!(js.Global().Get("process").Type() == js.TypeObject &&
		strings.HasPrefix(js.Global().Get("process").Get("argv0").String(), "node"))

// refuseRedirects makes every request through rt use fetch's redirect mode "error" when fetch is
// the path: a redirect fails the request. Not "manual": fetch then answers an opaque response with
// no status or Location, so Go could not apply its own policy either. Set on every request,
// overriding the caller's, so no plumbing client can be made to follow a hop Go doesn't see.
func refuseRedirects(rt http.RoundTripper) http.RoundTripper {
	if !usesFetch {
		return rt
	}
	return noRedirects{rt: rt}
}

type noRedirects struct{ rt http.RoundTripper }

func (n noRedirects) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context()) // a RoundTripper must not modify the caller's request
	r.Header.Set(jsFetchRedirect, "error")
	return n.rt.RoundTrip(r)
}
