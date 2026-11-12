//go:build !(js && wasm)

package dialguard

import (
	"context"
	"net"
)

// transportDial is the DialContext Transport installs: the guarded dialer's. On js/wasm it is nil
// instead (transport_dial_js.go).
func transportDial() func(ctx context.Context, network, address string) (net.Conn, error) {
	return Dialer().DialContext
}
