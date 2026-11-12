package dialguard

import (
	"errors"
	"strings"
	"testing"
)

// The arms Diego specified on snowplow#1131, each denied range in BOTH address families, plus the
// IPv4-mapped IPv6 spelling of each denied v4 range — which is the edge most likely to be missed,
// because ::ffff:169.254.169.254 is a different 16 bytes from 169.254.169.254 and only survives
// because To4() is consulted first.
func TestDenyAddr_DeniedRanges(t *testing.T) {
	denied := []struct {
		addr   string
		reason string
	}{
		// link-local / cloud metadata
		{"169.254.169.254:80", "link-local"},
		{"169.254.0.1:443", "link-local"},
		{"[fe80::1]:80", "link-local"},
		{"[fe80::dead:beef]:5000", "link-local"},
		// IPv4-mapped forms of the same
		{"[::ffff:169.254.169.254]:80", "link-local"},
		{"[::ffff:a9fe:a9fe]:80", "link-local"},
		// unspecified
		{"0.0.0.0:8080", "unspecified"},
		{"[::]:8080", "unspecified"},
		{"[::ffff:0.0.0.0]:8080", "unspecified"},
		// multicast
		{"224.0.0.1:1234", "multicast"},
		{"239.255.255.250:1900", "multicast"},
		{"[ff02::1]:1234", "multicast"},
		{"[ff01::1]:1234", "multicast"},
		{"[::ffff:224.0.0.1]:1234", "multicast"},
	}

	if len(denied) == 0 {
		t.Fatal("no cases — every assertion below would pass over an empty set")
	}

	for _, c := range denied {
		err := DenyAddr(c.addr)
		if err == nil {
			t.Errorf("%s was PERMITTED. This is the policy's whole purpose: 169.254.169.254 is the "+
				"cloud metadata service and a plain GET of it can return a token", c.addr)
			continue
		}
		var d *ErrDenied
		if !errors.As(err, &d) {
			t.Errorf("%s was refused with %T, not *ErrDenied — a caller cannot tell a policy "+
				"refusal from a network failure, and the two have opposite remedies", c.addr, err)
			continue
		}
		if !strings.Contains(d.Reason, c.reason) {
			t.Errorf("%s refused for %q, expected a reason mentioning %q. An operator told only "+
				"'refused' goes looking for a connectivity problem", c.addr, d.Reason, c.reason)
		}
	}
}

// THE CONTROL, and without it every assertion above is satisfied by a function that refuses
// everything. These must be PERMITTED, and the first two are not hypothetical: Krateo's own chart
// registry is oci://installer-core-provider-registry.krateo-system.svc.cluster.local:5000, which
// resolves into RFC-1918. A policy that denied these would break every installation.
func TestDenyAddr_AllowedRanges(t *testing.T) {
	allowed := []string{
		"10.96.1.10:5000",      // RFC-1918 — the in-cluster registry's range
		"172.16.0.9:5000",      // RFC-1918
		"192.168.1.5:443",      // RFC-1918
		"127.0.0.1:5000",       // loopback — allowed by the ruling
		"[::1]:5000",           // loopback v6
		"[fd00::1]:443",        // ULA
		"140.82.121.4:443",     // public (github)
		"[2606:50c0::153]:443", // public v6
	}

	if len(allowed) == 0 {
		t.Fatal("no cases — the control would be vacuous")
	}

	for _, a := range allowed {
		if err := DenyAddr(a); err != nil {
			t.Errorf("%s was REFUSED: %v. Loopback and private ranges are allowed by the ruling "+
				"precisely because in-cluster services and on-prem endpoints live there", a, err)
		}
	}
}

// FAILS CLOSED. Both arms, because the two inputs reach the same wrong outcome by different routes
// and "could not determine" must not read as "permitted".
func TestDenyAddr_FailsClosedOnAnythingItCannotPolice(t *testing.T) {
	t.Run("an unparseable address is refused", func(t *testing.T) {
		for _, bad := range []string{"", "not-an-address", "169.254.169.254", "[::1]"} {
			if err := DenyAddr(bad); err == nil {
				t.Errorf("%q was permitted. An address this function cannot parse is one it cannot "+
					"police, and permitting it makes the policy silently absent", bad)
			}
		}
	})

	// A NAME rather than an IP means resolution has not happened, so nothing has been checked. This
	// is unreachable from Control — the dialer resolves first — and refusing it is what keeps a
	// future caller that pre-checks a hostname from believing it got an answer.
	t.Run("a hostname is refused rather than resolved here", func(t *testing.T) {
		err := DenyAddr("metadata.google.internal:80")
		if err == nil {
			t.Fatal("a hostname was permitted. Resolving it here would reintroduce the rebinding " +
				"gap the dial-time check exists to close: the address checked must be the address " +
				"dialled")
		}
		var d *ErrDenied
		if errors.As(err, &d) && !strings.Contains(d.Reason, "not a resolved IP") {
			t.Errorf("the reason should say the input was not a resolved address, got %q", d.Reason)
		}
	})
}

// The constructors must actually install the hook. A Dialer() whose Control is nil would pass every
// test above — they exercise DenyAddr directly — while every real dial went unchecked, which is the
// exact shape of the gap this package was written to close.
func TestConstructorsInstallTheHook(t *testing.T) {
	if Dialer().Control == nil {
		t.Error("Dialer() returned a dialer with a nil Control: every dial through it is unchecked, " +
			"and the policy tests above would still pass")
	}
	c := Client(0)
	if c.Transport == nil {
		t.Error("Client() returned a client with a nil Transport, which means http.DefaultTransport " +
			"— the unguarded one this constructor exists to avoid")
	}
}
