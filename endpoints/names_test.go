package endpoints

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestExactSecretName(t *testing.T) {
	long := strings.Repeat("a", MaxExactUsernameLength)
	for _, tc := range []struct {
		username string
		want     string // "" when there is no exact name
	}{
		{"alice", "alice-clientconfig"},
		{"alice.smith", "alice.smith-clientconfig"},
		{"alice-smith", "alice-smith-clientconfig"},
		{"a1", "a1-clientconfig"},
		{"cdc-krateo-system-githubscaffoldingwithcompositionpages-v1-0-0-controller",
			"cdc-krateo-system-githubscaffoldingwithcompositionpages-v1-0-0-controller-clientconfig"},
		{long, long + "-clientconfig"},
		{long + "a", ""},
		{"", ""},
		{"Alice", ""},
		{"alice_smith", ""},
		{"alice@corp", ""},
		{".alice", ""},
		{"alice.", ""},
		{"alice..smith", ""},
		{"-alice", ""},
		{"alice-", ""},
		{"système", ""},
	} {
		got, ok := ExactSecretName(tc.username)
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("ExactSecretName(%q) = %q, %v; want %q", tc.username, got, ok, tc.want)
		}
		if ok {
			// Both of the user's Secrets are valid names.
			for _, n := range []string{got, tc.username + StepUpSuffix} {
				if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
					t.Errorf("%q isn't a valid Secret name: %v", n, errs)
				}
			}
		}
	}
}

func TestSessionSecretNames(t *testing.T) {
	for _, tc := range []struct {
		username string
		want     []string
	}{
		{"alice", []string{"alice-clientconfig"}},
		{"alice.smith", []string{"alice.smith-clientconfig", "alicesmith-clientconfig"}},
		{"Alice", []string{"alice-clientconfig"}}, // no exact name: legacy only
		{strings.Repeat("a", 64), []string{strings.Repeat("a", 64) + "-clientconfig", strings.Repeat("a", 63) + "-clientconfig"}},
	} {
		got := SessionSecretNames(tc.username)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("SessionSecretNames(%q) = %v, want %v", tc.username, got, tc.want)
		}
	}
}

// certData is base64 of a PEM client certificate whose CommonName is cn, as
// authn stores it.
func certData(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
}

func secret(name string, annotations map[string]string, cert []byte) *corev1.Secret {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
	if cert != nil {
		sec.Data = map[string][]byte{clientCertLabel: cert}
	}
	return sec
}

// The aliasing pair (snowplow's review of authn#140): john.doe's legacy name is
// johndoe-clientconfig, which is also johndoe's exact name. A Secret found
// under it is used only for the user it proves it belongs to, never the other.
func TestOwnsSecretNeverHandsOneUserAnothersCredential(t *testing.T) {
	const x, y = "john.doe", "johndoe"
	if SessionSecretNames(x)[1] != SessionSecretNames(y)[0] {
		t.Fatalf("the premise: %s's legacy name is %s's exact one", x, y)
	}
	shared := SecretName(x)

	for _, tc := range []struct {
		name   string
		sec    *corev1.Secret
		owners []string // who it belongs to
	}{
		{"annotated for y", secret(shared, map[string]string{UsernameAnnotation: y}, certData(t, y)), []string{y}},
		{"annotated for x", secret(shared, map[string]string{UsernameAnnotation: x}, certData(t, x)), []string{x}},
		{"legacy, certificate for y", secret(shared, nil, certData(t, y)), []string{y}},
		{"legacy, certificate for x", secret(shared, nil, certData(t, x)), []string{x}},
		// An annotation that disagrees with its certificate is no one's: the
		// reader would serve x's certificate to y (authn#147's review, L2).
		{"an annotation the certificate contradicts is no one's", secret(shared, map[string]string{UsernameAnnotation: y}, certData(t, x)), nil},
		{"annotated, with no certificate", secret(shared, map[string]string{UsernameAnnotation: y}, nil), []string{y}},
		{"annotated, with a certificate that doesn't parse", secret(shared, map[string]string{UsernameAnnotation: y}, []byte("garbage")), nil},
		{"an empty annotation is no one's", secret(shared, map[string]string{UsernameAnnotation: ""}, certData(t, y)), nil},
		{"no annotation, no certificate", secret(shared, nil, nil), nil},
		{"a certificate that doesn't parse", secret(shared, nil, []byte("not a certificate")), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, u := range []string{x, y} {
				want := false
				for _, o := range tc.owners {
					want = want || o == u
				}
				if got := OwnsSecret(tc.sec, u); got != want {
					t.Errorf("OwnsSecret(%s) = %v, want %v", u, got, want)
				}
			}
		})
	}
	if OwnsSecret(nil, x) || OwnsSecret(secret(shared, map[string]string{UsernameAnnotation: ""}, nil), "") {
		t.Error("a nil Secret, or an empty username, is never owned")
	}
}

// A certificate stored as the PEM block itself, not base64 of it, reads the
// same.
func TestOwnsSecretReadsAPEMCertificate(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(string(certData(t, "alice")))
	if err != nil {
		t.Fatal(err)
	}
	if !OwnsSecret(secret("alice-clientconfig", nil, raw), "alice") {
		t.Error("a PEM certificate for alice isn't alice's")
	}
}
