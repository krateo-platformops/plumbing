package endpoints

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// UsernameAnnotation records, on a session Secret, the exact username it holds
// the credential of (authn#140). A reader proves a Secret is the requester's
// with it (OwnsSecret) before using it, so a name two usernames share can never
// hand one of them the other's credential.
const UsernameAnnotation = "authn.krateo.io/username"

// StepUpSuffix ends the name of a user's step-up credential Secret, after
// ClientConfigSuffix: <name>-clientconfig-stepup. No session Secret's name ends
// in it, since those all end in ClientConfigSuffix.
const StepUpSuffix = ClientConfigSuffix + "-stepup"

// MaxExactUsernameLength is the longest username whose session and step-up
// Secrets can both be named after it exactly: a Secret's name is a DNS-1123
// subdomain of at most 253 characters, and the step-up suffix is the longer.
const MaxExactUsernameLength = validation.DNS1123SubdomainMaxLength - len(StepUpSuffix)

// ExactSecretName names username's session Secret after the username exactly,
// <username>-clientconfig, and reports whether it can: the username must be a
// DNS-1123 subdomain (lowercase a-z, 0-9, '-' and '.', each '.'-separated part
// starting and ending alphanumeric) of at most MaxExactUsernameLength, so that
// its step-up Secret is a valid name too. Dots are kept: alice.smith's Secret
// is alice.smith-clientconfig, not SecretName's alicesmith-clientconfig, which
// may be another user's (authn#140).
//
// It is the successor of SecretName, which stays as it is through the
// migration: readers try SessionSecretNames, and prove ownership with
// OwnsSecret.
func ExactSecretName(username string) (string, bool) {
	if username == "" || len(username) > MaxExactUsernameLength ||
		len(validation.IsDNS1123Subdomain(username)) > 0 {
		return "", false
	}
	return username + ClientConfigSuffix, true
}

// SessionSecretNames is the names username's session Secret may have, in the
// order a reader tries them: the exact name, when there is one, then the
// legacy one (SecretName), when it differs. A Secret found under either is
// used only once OwnsSecret proves it is username's.
func SessionSecretNames(username string) []string {
	legacy := SecretName(username)
	exact, ok := ExactSecretName(username)
	switch {
	case !ok:
		return []string{legacy}
	case exact == legacy:
		return []string{exact}
	default:
		return []string{exact, legacy}
	}
}

// OwnsSecret reports whether sec holds username's credential. A Secret
// written since authn#140 says so in UsernameAnnotation, which must equal
// username exactly. One written before, without the annotation, is
// username's when its client certificate's CommonName is username: authn
// issues the certificate to the username. Anything else is not username's,
// however it was found: alice.smith and alicesmith share the legacy name
// alicesmith-clientconfig, and a reader must never use one's credential for
// the other.
func OwnsSecret(sec *corev1.Secret, username string) bool {
	if sec == nil || username == "" {
		return false
	}
	if v, ok := sec.GetAnnotations()[UsernameAnnotation]; ok {
		return v == username
	}
	cn, ok := certificateCommonName(sec.Data[clientCertLabel])
	return ok && cn == username
}

// certificateCommonName is the CommonName of the client certificate a session
// Secret holds: base64 of a PEM block, as authn stores it (or the PEM block
// itself).
func certificateCommonName(data []byte) (string, bool) {
	raw := data
	if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data))); err == nil {
		raw = dec
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return "", false
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", false
	}
	return crt.Subject.CommonName, true
}
