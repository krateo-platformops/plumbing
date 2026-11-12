package usernames

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// The parts of a hashed Bounded result: p + boundedMarker + 12 hex digits.
const (
	boundedMarker  = "--"
	boundedHashLen = 12 // 48 bits of sha256
	// BoundedMinLen is the shortest maxLen Bounded accepts: a one-character
	// prefix, the marker and the hash.
	BoundedMinLen = 1 + len(boundedMarker) + boundedHashLen
)

// Bounded maps an issued username u to a DNS-1123 label of at most maxLen
// characters, for a consumer that needs one per username: the tenant policy
// names a tenant's namespace tenant-<Bounded(u, 56)> (acmp's tenant-policy
// design, §2.5). authn stamps it as the label authn.krateo.io/username-label.
//
//   - plain: u itself, when u is a DNS-1123 label of at most maxLen characters
//     with no "--". Every existing label-shaped username keeps its value.
//   - hashed: otherwise p + "--" + the first 12 hex digits of sha256(u), where
//     p is u with every character outside [a-z0-9-] replaced by '-', cut to
//     maxLen-14.
//
// Plain results never contain "--" and hashed ones always do, so the two sets
// are disjoint: no plain username can equal another user's hashed form. Two
// hashed results meet only if their names share the prefix and 48 bits of
// sha256.
//
// u is an issued username, so it starts with [a-z0-9] (endpoints.ExactSecretName
// accepts no other), and so does p. The algorithm and the bound are frozen:
// a change would label one person's new Secret differently from their old one,
// so it needs a versioned migration planned with the tenant-policy owner.
//
// maxLen must be at least BoundedMinLen and at most 63; Bounded panics
// otherwise, since the caller's constant is wrong.
func Bounded(u string, maxLen int) string {
	if maxLen < BoundedMinLen || maxLen > validation.DNS1123LabelMaxLength {
		panic(fmt.Sprintf("usernames.Bounded: maxLen %d is outside [%d, %d]", maxLen, BoundedMinLen, validation.DNS1123LabelMaxLength))
	}
	if len(u) <= maxLen && !strings.Contains(u, boundedMarker) && len(validation.IsDNS1123Label(u)) == 0 {
		return u
	}
	sum := sha256.Sum256([]byte(u))
	p := []byte(u)
	for i, c := range p {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			p[i] = '-'
		}
	}
	if max := maxLen - len(boundedMarker) - boundedHashLen; len(p) > max {
		p = p[:max]
	}
	return string(p) + boundedMarker + hex.EncodeToString(sum[:])[:boundedHashLen]
}
