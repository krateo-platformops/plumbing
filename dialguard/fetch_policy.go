//go:build !(js && wasm)

package dialguard

import "net/http"

// fetchPolicy is rt itself off js/wasm: Go follows redirects there, so CheckRedirect and the
// credential round-trippers see every hop, and there is no browser cookie jar to send from.
func fetchPolicy(rt http.RoundTripper) http.RoundTripper { return rt }
