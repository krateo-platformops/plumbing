package endpoints

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestDelegatedSecretName(t *testing.T) {
	long := strings.Repeat("a", 300)
	names := map[string]string{}
	for _, tc := range []struct{ username, actor string }{
		{"alice", "dispatcher"},
		{"alice", "ide-agent"},
		{"bob", "dispatcher"},
		{"Alice@Corp", "dispatcher"},
		{long, long},
		// The join can't be shifted: "a"+"bc" and "ab"+"c" differ.
		{"a", "bc"},
		{"ab", "c"},
	} {
		got := DelegatedSecretName(tc.username, tc.actor)
		if got != DelegatedSecretName(tc.username, tc.actor) {
			t.Errorf("DelegatedSecretName(%q, %q) isn't stable", tc.username, tc.actor)
		}
		if errs := validation.IsDNS1123Subdomain(got); len(errs) > 0 {
			t.Errorf("DelegatedSecretName(%q, %q) = %q isn't a valid Secret name: %v", tc.username, tc.actor, got, errs)
		}
		if !strings.HasPrefix(got, DelegatedSecretPrefix) {
			t.Errorf("DelegatedSecretName(%q, %q) = %q lacks the prefix", tc.username, tc.actor, got)
		}
		// Nothing that finds session Secrets by suffix finds it.
		if strings.HasSuffix(got, ClientConfigSuffix) || strings.HasSuffix(got, StepUpSuffix) {
			t.Errorf("DelegatedSecretName(%q, %q) = %q ends like a session Secret", tc.username, tc.actor, got)
		}
		pair := tc.username + " / " + tc.actor
		if other, ok := names[got]; ok {
			t.Errorf("%s and %s share %q", pair, other, got)
		}
		names[got] = pair
	}
	// Pinned: authn writes it and snowplow reads it, so it never changes.
	if got, want := DelegatedSecretName("alice", "dispatcher"),
		"delegated-"+sha256Hex("alice\x00dispatcher"); got != want {
		t.Errorf("DelegatedSecretName(alice, dispatcher) = %q, want %q", got, want)
	}
}

func delegatedCert(t *testing.T, cn string, orgs []string, notBefore, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1),
		Subject:   pkix.Name{CommonName: cn, Organization: orgs},
		NotBefore: notBefore, NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
}

func TestCheckDelegatedCredential(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	valid := func(cn string, orgs ...string) []byte {
		return delegatedCert(t, cn, orgs, now.Add(-time.Minute), now.Add(14*time.Minute))
	}
	ann := func(username, actor string) map[string]string {
		return map[string]string{UsernameAnnotation: username, ActorAnnotation: actor}
	}
	name := DelegatedSecretName("alice", "dispatcher")
	groups := []string{"devs", "ops"}

	for _, tc := range []struct {
		name   string
		sec    *corev1.Secret
		user   string
		actor  string
		groups []string
		want   DelegatedCredential
	}{
		{"held", secret(name, ann("alice", "dispatcher"), valid("alice", "devs", "ops")), "alice", "dispatcher", groups, DelegatedCredentialHeld},
		{"held, groups in another order and repeated", secret(name, ann("alice", "dispatcher"), valid("alice", "ops", "devs", "ops")), "alice", "dispatcher", groups, DelegatedCredentialHeld},
		{"held, no groups", secret(name, ann("alice", "dispatcher"), valid("alice")), "alice", "dispatcher", nil, DelegatedCredentialHeld},

		{"no Secret", nil, "alice", "dispatcher", groups, DelegatedCredentialNotOwned},
		{"no username", secret(name, ann("", "dispatcher"), valid("")), "", "dispatcher", groups, DelegatedCredentialNotOwned},
		{"no actor", secret(name, ann("alice", ""), valid("alice", "devs", "ops")), "alice", "", groups, DelegatedCredentialNotOwned},
		{"no annotations", secret(name, nil, valid("alice", "devs", "ops")), "alice", "dispatcher", groups, DelegatedCredentialNotOwned},
		{"another person's annotation", secret(name, ann("bob", "dispatcher"), valid("alice", "devs", "ops")), "alice", "dispatcher", groups, DelegatedCredentialNotOwned},
		{"another actor's annotation", secret(name, ann("alice", "ide-agent"), valid("alice", "devs", "ops")), "alice", "dispatcher", groups, DelegatedCredentialNotOwned},
		{"another person's certificate", secret(name, ann("alice", "dispatcher"), valid("bob", "devs", "ops")), "alice", "dispatcher", groups, DelegatedCredentialNotOwned},
		{"no certificate", secret(name, ann("alice", "dispatcher"), nil), "alice", "dispatcher", groups, DelegatedCredentialNotOwned},
		{"unreadable certificate", secret(name, ann("alice", "dispatcher"), []byte("not a certificate")), "alice", "dispatcher", groups, DelegatedCredentialNotOwned},

		{"expired", secret(name, ann("alice", "dispatcher"), delegatedCert(t, "alice", groups, now.Add(-20*time.Minute), now.Add(-time.Second))), "alice", "dispatcher", groups, DelegatedCredentialExpired},
		{"expires exactly now", secret(name, ann("alice", "dispatcher"), delegatedCert(t, "alice", groups, now.Add(-20*time.Minute), now)), "alice", "dispatcher", groups, DelegatedCredentialExpired},
		{"not yet valid", secret(name, ann("alice", "dispatcher"), delegatedCert(t, "alice", groups, now.Add(time.Second), now.Add(15*time.Minute))), "alice", "dispatcher", groups, DelegatedCredentialExpired},
		// Expiry is reported before groups: an expired credential is
		// expired, whatever it carries.
		{"expired, other groups", secret(name, ann("alice", "dispatcher"), delegatedCert(t, "alice", []string{"devs"}, now.Add(-20*time.Minute), now.Add(-time.Second))), "alice", "dispatcher", groups, DelegatedCredentialExpired},

		{"a group fewer", secret(name, ann("alice", "dispatcher"), valid("alice", "devs")), "alice", "dispatcher", groups, DelegatedCredentialMismatch},
		{"a group more", secret(name, ann("alice", "dispatcher"), valid("alice", "devs", "ops", "admins")), "alice", "dispatcher", groups, DelegatedCredentialMismatch},
		{"groups but the token has none", secret(name, ann("alice", "dispatcher"), valid("alice", "devs")), "alice", "dispatcher", nil, DelegatedCredentialMismatch},
		{"groups differ in case", secret(name, ann("alice", "dispatcher"), valid("alice", "Devs", "ops")), "alice", "dispatcher", groups, DelegatedCredentialMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckDelegatedCredential(tc.sec, tc.user, tc.actor, tc.groups, now); got != tc.want {
				t.Errorf("CheckDelegatedCredential = %v, want %v", got, tc.want)
			}
		})
	}
}

// A delegated credential Secret isn't a session Secret: OwnsSecret, which
// knows nothing of actors, must not be what admits it, and the reader's
// check must not admit a session Secret.
func TestCheckDelegatedCredentialRefusesASessionSecret(t *testing.T) {
	now := time.Now()
	session := secret("alice-clientconfig", map[string]string{UsernameAnnotation: "alice"},
		delegatedCert(t, "alice", []string{"devs"}, now.Add(-time.Minute), now.Add(time.Hour)))
	if !OwnsSecret(session, "alice") {
		t.Fatal("control: the session Secret should be alice's")
	}
	if got := CheckDelegatedCredential(session, "alice", "dispatcher", []string{"devs"}, now); got != DelegatedCredentialNotOwned {
		t.Errorf("CheckDelegatedCredential(session Secret) = %v, want not-owned", got)
	}
}

func TestDelegatedCredentialString(t *testing.T) {
	for c, want := range map[DelegatedCredential]string{
		DelegatedCredentialHeld:     "held",
		DelegatedCredentialNotOwned: "not-owned",
		DelegatedCredentialExpired:  "expired",
		DelegatedCredentialMismatch: "mismatch",
		DelegatedCredential(99):     "unknown",
	} {
		if got := c.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(c), got, want)
		}
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestIsDelegatedSecretName(t *testing.T) {
	digest := sha256Hex("alice\x00dispatcher")
	for name, want := range map[string]bool{
		DelegatedSecretName("alice", "dispatcher"): true,
		DelegatedSecretName("", ""):                true,
		"delegated-" + digest:                      true,
		"delegated-" + strings.ToUpper(digest):     false,
		"delegated-" + digest[:63]:                 false,
		"delegated-" + digest + "0":                false,
		"delegated-" + digest[:63] + "g":           false,
		"delegated-":                               false,
		digest:                                     false,
		"delegated-alice-clientconfig":             false,
		"alice-clientconfig":                       false,
		"alice-clientconfig-stepup":                false,
		"x-delegated-" + digest:                    false,
		"":                                         false,
	} {
		if got := IsDelegatedSecretName(name); got != want {
			t.Errorf("IsDelegatedSecretName(%q) = %v, want %v", name, got, want)
		}
	}
}
