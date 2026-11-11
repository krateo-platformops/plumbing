//go:build !(js && wasm)

package dialguard

import "testing"

func TestTransportInstallsTheHook(t *testing.T) {
	if Transport().DialContext == nil {
		t.Error("Transport() returned a transport with a nil DialContext, so it falls back to the " +
			"zero-value dialer and the hook is bypassed")
	}
}
