//go:build !(js && wasm)

package dialguard

import "net/http"

// refuseRedirects is rt itself off js/wasm: Go follows redirects there, so CheckRedirect and the
// credential round-trippers see every hop.
func refuseRedirects(rt http.RoundTripper) http.RoundTripper { return rt }
