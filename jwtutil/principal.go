package jwtutil

import "encoding/json"

// Principal is the kind of identity a token names. authn sets it from how the
// token was obtained, never from anything the client sends, so a validator
// can tell a person from a workload without looking anything up.
type Principal string

const (
	// PrincipalHuman is a person who signed in (basic, LDAP, OAuth, OIDC).
	PrincipalHuman Principal = "human"
	// PrincipalAgent is an AI agent acting as itself.
	PrincipalAgent Principal = "agent"
	// PrincipalController is a controller reconciling resources, such as a
	// composition controller.
	PrincipalController Principal = "controller"
	// PrincipalService is any other workload, and the default for a service
	// that doesn't declare its kind.
	PrincipalService Principal = "service"
)

// Known reports whether p is one of the four kinds above.
func (p Principal) Known() bool {
	switch p {
	case PrincipalHuman, PrincipalAgent, PrincipalController, PrincipalService:
		return true
	}
	return false
}

// UnmarshalJSON reads a known kind as itself and anything else (an unknown
// string, a number, null) as "". A validator therefore never fails on the
// claim: a token from an older issuer has none, one from a newer issuer may
// carry a kind this version doesn't know, and both read as "", which callers
// treat as unknown.
func (p *Principal) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil || !Principal(s).Known() {
		*p = ""
		return nil
	}
	*p = Principal(s)
	return nil
}

// ActorPrincipal returns the kind of the identity performing the request: the
// outermost actor's on a delegated token, otherwise the token's own. "" means
// the issuer didn't say.
func (u UserInfo) ActorPrincipal() Principal {
	if u.Act != nil {
		return u.Act.Principal
	}
	return u.Principal
}

// validPrincipals reports whether the token's kind and every actor's kind is
// either unset or known. CreateToken refuses anything else, so an issuer
// can't sign a kind its validators would read as "".
func validPrincipals(p Principal, a *Actor) bool {
	if p != "" && !p.Known() {
		return false
	}
	for ; a != nil; a = a.Act {
		if a.Principal != "" && !a.Principal.Known() {
			return false
		}
	}
	return true
}
