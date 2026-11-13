package jwtutil

import (
	"crypto/rsa"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	defaultISS = "krateo.io"
)

type UserInfo struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
	// Act is RFC 8693's actor claim: present only on a delegated token, it
	// names the identity acting for Username (the subject). Nil on every
	// other token.
	Act *Actor `json:"act,omitempty"`
	// Principal is the kind of identity Username is. Empty on a token from
	// an issuer that doesn't set it, and omitted from the token then.
	Principal Principal `json:"principal,omitempty"`
}

// Actor is one link of RFC 8693's "act" claim (§4.1). Sub is the acting
// identity; Act, when set, is the actor that delegated to it, so the outermost
// Actor is the current actor and each nested one is earlier in the chain.
type Actor struct {
	Sub string `json:"sub"`
	Act *Actor `json:"act,omitempty"`
	// Principal is the kind of identity Sub is, as an extra actor claim
	// (RFC 8693 §4.1 allows them).
	Principal Principal `json:"principal,omitempty"`
}

// MaxActorDepth bounds an "act" chain. A deeper chain is refused when
// creating a token and when validating one.
const MaxActorDepth = 8

// Actor returns the identity performing the request: the outermost actor of a
// delegated token, or Username when the token isn't delegated.
func (u UserInfo) Actor() string {
	if u.Act != nil {
		return u.Act.Sub
	}
	return u.Username
}

// DelegationChain returns every identity on the token, current actor first and
// ending with the subject. A token that isn't delegated returns just
// Username.
func (u UserInfo) DelegationChain() []string {
	var chain []string
	for a := u.Act; a != nil; a = a.Act {
		chain = append(chain, a.Sub)
	}
	return append(chain, u.Username)
}

// validActor reports whether every link of the chain names an actor and the
// chain is no deeper than MaxActorDepth. A nil chain is valid.
func validActor(a *Actor) bool {
	for depth := 0; a != nil; a = a.Act {
		depth++
		if a.Sub == "" || depth > MaxActorDepth {
			return false
		}
	}
	return true
}

type KrateoClaims struct {
	UserInfo
	jwt.RegisteredClaims
}

type CreateTokenOptions struct {
	Username   string
	Groups     []string
	Duration   time.Duration
	KeyID      string
	PrivateKey *rsa.PrivateKey
	// Act makes the token a delegated one (RFC 8693): Username stays the
	// subject, and Act names who acts for it. Nil for an ordinary token.
	Act *Actor
	// Principal is the kind of identity Username is. Empty leaves the claim
	// out, as before it existed.
	Principal Principal
	// Audience is the token's "aud" (RFC 7519 §4.1.3): the recipients it is
	// meant for, such as "agentiko" for a token exchanged for one service.
	// Empty leaves the claim out, as before it existed. The validators here
	// don't check it, so a token carrying one is accepted everywhere a token
	// without one is; a recipient opts into checking it.
	Audience []string
}

// CreateToken generates a signed JWT token using the provided
// username, group list, and expiration duration.
// The token is signed asymmetrically using the RS256 algorithm and the
// supplied RSA private key, and carries a "kid" header identifying the key
// so that JWKS-based validators (e.g. agentgateway) can select the matching
// public key. The corresponding public key must be published in the JWKS
// under the same KeyID.
func CreateToken(opts CreateTokenOptions) (string, error) {
	if opts.PrivateKey == nil {
		return "", fmt.Errorf("private key cannot be nil")
	}

	if opts.KeyID == "" {
		return "", fmt.Errorf("key ID cannot be empty")
	}

	if opts.Username == "" {
		return "", fmt.Errorf("username cannot be empty")
	}

	if !validActor(opts.Act) {
		return "", fmt.Errorf("act must name an actor at every level, at most %d deep", MaxActorDepth)
	}

	if !validPrincipals(opts.Principal, opts.Act) {
		return "", fmt.Errorf("principal must be empty or one of human, agent, controller, service, on the token and on every actor")
	}

	now := time.Now()

	claims := KrateoClaims{
		UserInfo: UserInfo{
			Username:  opts.Username,
			Groups:    opts.Groups,
			Act:       opts.Act,
			Principal: opts.Principal,
		},
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    defaultISS,
			ExpiresAt: jwt.NewNumericDate(now.Add(opts.Duration)),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Subject:   opts.Username,
		},
	}
	if len(opts.Audience) > 0 {
		for _, a := range opts.Audience {
			if a == "" {
				return "", fmt.Errorf("an audience cannot be empty")
			}
		}
		claims.Audience = jwt.ClaimStrings(opts.Audience)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = opts.KeyID

	return token.SignedString(opts.PrivateKey)
}
