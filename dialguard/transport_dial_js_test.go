//go:build js && wasm

package dialguard

import "testing"

// On js/wasm, Transport must take net/http's Fetch API path, which it does only with no dial
// function of its own (net/http/roundtrip_js.go). With one, every request would dial Go's
// in-process network, which refuses a dialer carrying Control outright.
func TestTransportOnJSHasNoDialSoFetchIsUsed(t *testing.T) {
	tr := Transport()
	if tr.DialContext != nil || tr.Dial != nil || tr.DialTLSContext != nil || tr.DialTLS != nil {
		t.Error("Transport() on js/wasm sets a dial function: net/http then dials the in-process " +
			"network instead of using fetch, and no request reaches the browser")
	}
}

// Dialer is unchanged on js/wasm: a dial through it still carries Control and fails closed.
func TestDialerOnJSStillCarriesControl(t *testing.T) {
	if Dialer().Control == nil {
		t.Error("Dialer() lost its Control hook on js/wasm")
	}
}
