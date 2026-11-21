package endpoints

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// ActorAnnotation records, on a delegated credential Secret, the actor it
// was issued to act through (authn#167): the delegated token's act.sub, the
// name of the actor's ServiceAccount mapping.
const ActorAnnotation = "authn.krateo.io/actor"

// DelegatedSecretPrefix starts the name of every delegated credential
// Secret. Those names end in hex, so none is a session Secret's (those end in
// ClientConfigSuffix) or a step-up Secret's (StepUpSuffix), and nothing that
// scans session Secrets by suffix finds one.
const DelegatedSecretPrefix = "delegated-"

// DelegatedSecretName names the Secret holding the credential authn issues
// with a delegated token (authn#167): the one an actor uses to reach the
// apiserver as username. There is one per person and actor, each exchange by
// the actor for the person replacing it, so two actors never replace each
// other's.
//
// username is the token's username. actor is the actor the token was issued
// to: its outermost act.sub, the name of the actor's ServiceAccount mapping
// (jwtutil's UserInfo.Actor()), never a deeper link of the chain.
//
// The name is DelegatedSecretPrefix and the hex SHA-256 of username's length
// in decimal, a colon, username, then actor. The length prefix makes the
// encoding injective whatever the two contain, NUL included: two distinct
// (username, actor) pairs never share a name. It fits any username and actor,
// and authn and a reader compute the same one. A reader still proves the
// Secret is the token's with CheckDelegatedCredential before using it.
func DelegatedSecretName(username, actor string) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(len(username)) + ":" + username + actor))
	return DelegatedSecretPrefix + hex.EncodeToString(sum[:])
}

// IsDelegatedSecretName reports whether name is a delegated credential
// Secret's: DelegatedSecretPrefix and 64 lowercase hex digits, the shape
// DelegatedSecretName gives. A reader uses it to keep these Secrets out of
// whatever it does with session Secrets, and to refuse any reference to one
// that isn't the delegated token's own.
func IsDelegatedSecretName(name string) bool {
	digest, ok := strings.CutPrefix(name, DelegatedSecretPrefix)
	if !ok || len(digest) != hex.EncodedLen(sha256.Size) {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// DelegatedCredential is what a Secret holds for a delegated token: the
// outcome of CheckDelegatedCredential. A reader serves the Secret's credential
// only on DelegatedCredentialHeld; every other value refuses the token, and
// the client exchanges again.
type DelegatedCredential int

const (
	// DelegatedCredentialHeld: the Secret is the token's credential, valid
	// now.
	DelegatedCredentialHeld DelegatedCredential = iota
	// DelegatedCredentialNotOwned: there is no Secret, or it isn't the
	// credential authn issued to this person through this actor. Its
	// annotations, or its certificate's CommonName, name someone else, or
	// it holds no readable client certificate.
	DelegatedCredentialNotOwned
	// DelegatedCredentialExpired: the person's and actor's credential, but
	// its certificate isn't valid now.
	DelegatedCredentialExpired
	// DelegatedCredentialMismatch: the person's and actor's credential,
	// valid now, but its certificate's Organizations aren't the token's
	// groups. Another exchange rewrote it, or the issuance is broken.
	DelegatedCredentialMismatch
)

// String names the outcome, for logs.
func (c DelegatedCredential) String() string {
	switch c {
	case DelegatedCredentialHeld:
		return "held"
	case DelegatedCredentialNotOwned:
		return "not-owned"
	case DelegatedCredentialExpired:
		return "expired"
	case DelegatedCredentialMismatch:
		return "mismatch"
	}
	return "unknown"
}

// CheckDelegatedCredential reports what sec holds for a delegated token
// naming username, groups and actor, at now. It is held only when all of
// these are true:
//   - UsernameAnnotation equals username and ActorAnnotation equals actor;
//   - sec holds a client certificate whose CommonName is username;
//   - that certificate is valid at now;
//   - its Organizations are exactly groups, compared as sets.
//
// The apiserver then authorizes the same identity the reader authorized
// the token as. The checks run in that order, and the first one that fails
// is the outcome. A Secret a newer exchange rewrote with other groups is
// DelegatedCredentialMismatch: the token is refused, never served another
// identity's groups.
func CheckDelegatedCredential(sec *corev1.Secret, username, actor string, groups []string, now time.Time) DelegatedCredential {
	if sec == nil || username == "" || actor == "" {
		return DelegatedCredentialNotOwned
	}
	ann := sec.GetAnnotations()
	if ann[UsernameAnnotation] != username || ann[ActorAnnotation] != actor {
		return DelegatedCredentialNotOwned
	}
	crt, ok := clientCertificate(sec.Data[clientCertLabel])
	if !ok || crt.Subject.CommonName != username {
		return DelegatedCredentialNotOwned
	}
	if now.Before(crt.NotBefore) || !now.Before(crt.NotAfter) {
		return DelegatedCredentialExpired
	}
	if !sameSet(crt.Subject.Organization, groups) {
		return DelegatedCredentialMismatch
	}
	return DelegatedCredentialHeld
}

// sameSet reports whether a and b hold the same strings, ignoring order and
// repeats.
func sameSet(a, b []string) bool {
	x := slices.Compact(slices.Sorted(slices.Values(a)))
	y := slices.Compact(slices.Sorted(slices.Values(b)))
	return slices.Equal(x, y)
}
