// Package dialguard refuses outbound connections to special-purpose addresses, checked on the
// address ACTUALLY DIALLED.
//
// Policy, ruled by Diego on snowplow#1131 (2026-11-04 evening, replacing an earlier "refuse
// nothing") and shared verbatim with snowplow so the two components cannot diverge:
//
//	REFUSED, on every request, with or without credentials:
//	  169.254.0.0/16, fe80::/10   cloud metadata and link-local
//	  0.0.0.0, ::                 unspecified
//	  224.0.0.0/4, ff00::/8       multicast
//
//	ALLOWED: loopback and private ranges (RFC-1918, ULA). In-cluster services -- including
//	Krateo's own registry at *.svc.cluster.local -- and on-prem endpoints live there, so denying
//	them would break every installation.
//
// NOT GATED ON CREDENTIALS, and that is a deliberate departure from the first draft of this. The
// metadata service is an SSRF target with no credential involved: a request that merely FETCHES
// 169.254.169.254 can return a cloud token. Gating on credentials would have left that open while
// looking like a fix.
//
// CHECKED AT DIAL TIME ON THE RESOLVED ADDRESS, via net.Dialer.Control, rather than on the
// hostname at admission. Three consequences, each of which is why this is the right hook:
//
//  1. DNS REBINDING is closed. A name that resolves to a public address when validated and to
//     169.254.169.254 when dialled is refused, because the check sees the second answer. A
//     hostname check sees only the first.
//  2. IDNA AND HOSTNAME SPELLING become irrelevant. Confusables, trailing dots, uppercase,
//     percent-encoding and every other spelling of a name resolve to the same address, and the
//     address is what is checked. A name-based allowlist has to normalise correctly to be sound;
//     this has nothing to normalise.
//  3. REDIRECTS are covered without a second mechanism, because every hop dials.
//
// FAILS CLOSED. An unresolvable or unparseable host never produces a dial, so the request fails
// rather than proceeding -- and DenyAddr refuses anything it cannot parse rather than passing it
// through. "Could not determine" must not read as "permitted".
//
// NO KNOB AND NO ALLOWLIST, by the same ruling. An escape hatch here is a setting whose only
// users are the people it exists to stop.
package dialguard

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrDenied is returned for a refused destination. A sentinel so a caller can distinguish a policy
// refusal from a network failure: the two have opposite remedies, and conflating them sends an
// operator to check connectivity for a destination that will never be permitted.
type ErrDenied struct {
	Addr   string
	Reason string
}

func (e *ErrDenied) Error() string {
	return fmt.Sprintf("refusing to dial %s: %s (destination policy, snowplow#1131 / "+
		"core-provider#306; there is no allowlist to add it to)", e.Addr, e.Reason)
}

// Control is the net.Dialer.Control hook. Install it on EVERY dialer that can carry a
// user-influenced destination.
//
// The address is post-resolution: net.Dialer resolves the name and calls Control once per candidate
// address, immediately before connecting to that address. Returning an error refuses that dial.
func Control(network, address string, _ syscall.RawConn) error {
	return DenyAddr(address)
}

// DenyAddr reports whether an already-resolved "host:port" may be dialled, as a separate exported
// function so the policy is testable without a syscall.RawConn and so a caller can pre-check.
func DenyAddr(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		// A dialer always passes host:port, so this is unreachable in the Control path. Refused
		// rather than allowed anyway: an address this function cannot parse is one it cannot
		// police, and the whole point is that uncertainty does not become permission.
		return &ErrDenied{Addr: address, Reason: "the address could not be parsed into host and port"}
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// Also unreachable from Control -- the host is an IP by then. A NAME arriving here means
		// resolution did not happen, so nothing has been checked.
		return &ErrDenied{Addr: address, Reason: fmt.Sprintf(
			"%q is not a resolved IP address, so the destination policy could not be applied", host)}
	}

	// IPv4-MAPPED IPv6 IS NORMALISED FIRST, and this is the edge most likely to be missed. Go's
	// net.IP predicates handle ::ffff:a.b.c.d correctly because they consult To4() internally --
	// but relying on that silently is what makes a future refactor dangerous, so the mapping is
	// made explicit here and asserted in the tests for every denied range.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	switch {
	case ip.IsUnspecified():
		// 0.0.0.0 and ::. Dialling the unspecified address reaches a local service on most
		// platforms, so it is a loopback alias with a different spelling.
		return &ErrDenied{Addr: address, Reason: "the unspecified address (0.0.0.0 / ::)"}

	case ip.IsLinkLocalUnicast():
		// 169.254.0.0/16 and fe80::/10. This is the one that matters: 169.254.169.254 is the cloud
		// metadata service on AWS, GCP and Azure, and a plain GET of it can return a token.
		return &ErrDenied{Addr: address, Reason: "a link-local address (169.254.0.0/16, fe80::/10) " +
			"— this range holds the cloud metadata service"}

	case ip.IsMulticast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		// 224.0.0.0/4 and ff00::/8. IsMulticast covers both, and the two narrower predicates are
		// named as well so removing one does not silently widen what is permitted.
		return &ErrDenied{Addr: address, Reason: "a multicast address (224.0.0.0/4, ff00::/8)"}
	}

	// LOOPBACK AND PRIVATE FALL THROUGH DELIBERATELY. Not an omission: Krateo's own chart registry
	// is oci://installer-core-provider-registry.krateo-system.svc.cluster.local:5000, which
	// resolves into RFC-1918, and denying it would break every installation. Recorded here because
	// the absence of IsLoopback/IsPrivate from the switch above is the first thing a reader will
	// take for a bug.
	return nil
}

// Dialer returns a net.Dialer with Control installed and the timeouts this repo already used at
// its three dial sites, so wiring it in is not also a behaviour change.
//
// A CONSTRUCTOR RATHER THAN A DOCUMENTED FIELD TO SET. Three sites built a dialer independently
// and a fourth and fifth had none at all and fell through to http.DefaultTransport -- which is the
// failure the ruling warns about: "a check in a builder that some call sites bypass looks complete
// and isn't". A constructor plus the AST guard in dialguard_wiring_test.go makes the omission a
// test failure rather than an invisible gap.
func Dialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   Control,
	}
}

// Transport returns an *http.Transport whose DialContext is guarded, with the pooling settings the
// existing sites shared. Callers that need to vary a field mutate the result.
//
// On js/wasm (a program running in a browser) it has no DialContext, so its requests go through
// the browser's fetch and the policy does not apply: transport_dial_js.go says why. There fetch
// also follows redirects itself, so a client built on it must send through RoundTripper.
func Transport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           transportDial(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// Client returns an *http.Client with a guarded transport. Exists so no caller has to reach for
// http.DefaultClient, which has an unguarded transport and is shared process-wide.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: RoundTripper(Transport()), Timeout: timeout}
}
