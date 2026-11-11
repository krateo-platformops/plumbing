// Package usernames holds the static rules for the usernames authn issues, so
// every component that names a user (authn's name guard, an access-request
// form, an approval controller) decides from one definition rather than a copy.
//
// The rules here need no cluster: they are what a public form may check. Whether
// a name is already claimed, provisioned or granted rights by a binding is
// decided in-cluster, by authn's guard and at an administrator's approval, never
// on a public endpoint, so nobody can enumerate who exists.
package usernames

import (
	"errors"
	"fmt"
	"strings"

	"github.com/krateo-platformops/plumbing/kubeutil"
)

// reservedNames and reservedPrefixes are usernames an identity source (LDAP,
// OAuth, OIDC) may never take, whether or not any binding names them yet: the
// names Kubernetes and the managed platforms give their own components, and
// administrator names. A cluster may grant them rights at any time.
//
// The colon form, "krateo:<name>", is reserved for platform identities authn
// never issues: a component impersonates one (snowplow's "krateo:public" for
// token-less public widgets) and relies on no person ever signing in as it. A
// name an identity source issues as-is (LDAP's uid) must therefore never be
// one of them.
var (
	reservedNames    = []string{"admin", "administrator", "root", "masterclient", "nodeclient", "aksservice"}
	reservedPrefixes = []string{"system", "kube-", "kubelet", "cluster-", "gke-", "eks-", "aks-", "krateo:"}
)

// ReservedNames returns the exact usernames that are reserved.
func ReservedNames() []string { return append([]string(nil), reservedNames...) }

// ReservedPrefixes returns the prefixes that reserve every username they start.
func ReservedPrefixes() []string { return append([]string(nil), reservedPrefixes...) }

// Reserved reports whether an identity source may not take username.
func Reserved(username string) bool {
	for _, n := range reservedNames {
		if username == n {
			return true
		}
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(username, p) {
			return true
		}
	}
	return false
}

// Normalize is the username authn issues for an identity source's name: it
// makes the name DNS-1123 compatible (kubeutil.MakeDNS1123Compatible), as the
// OAuth and OIDC logins do with preferred_username.
func Normalize(name string) string { return kubeutil.MakeDNS1123Compatible(name) }

// The reasons Check refuses a username.
var (
	ErrEmpty     = errors.New("the username is empty")
	ErrNotIssued = errors.New("authn would issue the username in another form")
	ErrReserved  = errors.New("the username is reserved")
	ErrTooLong   = errors.New("the username is longer than authn issues")
)

// MaxLength is the longest username authn issues for an identity source:
// Normalize cuts a longer one to this length, so it would be issued as another
// name.
const MaxLength = kubeutil.DNS1123NameMaximumLength

// Check reports whether an identity source can be given username as it is:
// non-empty, at most MaxLength, already in the form authn issues (so the name a
// person asks for is the name they sign in as, and its credential Secret is
// endpoints.SecretName(username)), and not reserved. It reads no cluster state.
// A component may add its own limits on top (a tenant namespace named after the
// user, say).
func Check(username string) error {
	switch {
	case username == "":
		return ErrEmpty
	case len(username) > MaxLength:
		return ErrTooLong
	case Normalize(username) != username:
		return fmt.Errorf("%w: %q", ErrNotIssued, Normalize(username))
	case Reserved(username):
		return ErrReserved
	}
	return nil
}
