//go:build js && wasm

package dialguard

import (
	"context"
	"net"
)

// transportDial is nil on js/wasm, so a Transport sends its requests through the browser's fetch:
// net/http takes the Fetch API path only when a Transport has no dial function of its own
// (net/http/roundtrip_js.go). With one, it dials Go's in-process network, where nothing outside the
// program listens, and a dialer carrying a Control hook, as Dialer's does, is refused there outright
// (ENOTSUP), so every request through Transport would fail.
//
// THE POLICY DOES NOT APPLY ON THIS BUILD. fetch connects on its own and Go never sees the
// address, so there is nothing for Control to check; the browser's own rules for where a page may
// connect apply instead. Dialer and Control are unchanged: a dial through them still fails closed.
// TLS client certificates (TLSClientConfig.Certificates) are not presented by fetch, and server
// certificates are verified by the browser against its own roots, not TLSClientConfig.RootCAs.
func transportDial() func(ctx context.Context, network, address string) (net.Conn, error) {
	return nil
}
