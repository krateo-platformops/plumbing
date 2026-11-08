package jwtutil

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrTokenExpired = errors.New("token is expired")
	ErrTokenInvalid = errors.New("token is invalid")
)

// Validate parses and verifies the given bearer token against the supplied
// RSA public key. Only the RS256 asymmetric algorithm is accepted; tokens
// signed with any other method (in particular symmetric HMAC) are rejected to
// prevent algorithm-confusion attacks.
//
// Prefer ValidateWithKeySource when the key comes from authn's JWKS endpoint;
// this form is for callers holding a single key already.
func Validate(publicKey *rsa.PublicKey, bearer string) (UserInfo, error) {
	if publicKey == nil {
		return UserInfo{}, fmt.Errorf("public key cannot be nil")
	}

	return ValidateWithKeySource(NewStaticKeySource(publicKey), bearer)
}

// ValidateWithKeySource parses and verifies the given bearer token, resolving
// the verification key through keys using the token's "kid" header. This is the
// JWKS-friendly form: a JWKSKeySource fetches authn's published key set and
// caches it, so key rotation needs no redeploy of the validating component.
//
// As with Validate, only RS256 is accepted — a token offering any other
// algorithm (notably symmetric HMAC) is rejected before the key is consulted,
// which is what makes algorithm confusion impossible here.
//
// Errors are separated by fault so callers can answer with the right status:
// ErrTokenExpired and ErrTokenInvalid mean the caller's token is at fault
// (401), whereas ErrKeyUnavailable means we could not obtain the key (503) and
// says nothing about the token.
func ValidateWithKeySource(keys KeySource, bearer string) (UserInfo, error) {
	claims, err := ValidateClaimsWithKeySource(keys, bearer)
	if err != nil {
		return UserInfo{}, err
	}
	return claims.UserInfo, nil
}

// ValidateClaimsWithKeySource verifies bearer exactly as ValidateWithKeySource
// does, with the same errors, and returns all of its verified claims: the
// identity and, unlike ValidateWithKeySource, the registered claims such as
// ExpiresAt. Use it when a decision depends on one of those, for example a
// token issued from this one that must not outlive it.
func ValidateClaimsWithKeySource(keys KeySource, bearer string) (KrateoClaims, error) {
	if keys == nil {
		return KrateoClaims{}, fmt.Errorf("key source cannot be nil")
	}

	tok, err := jwt.ParseWithClaims(bearer, &KrateoClaims{},
		func(token *jwt.Token) (any, error) {
			if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			// A non-string or absent kid becomes "", which a single-key set
			// still resolves; see JWKSKeySource.PublicKey.
			kid, _ := token.Header["kid"].(string)
			return keys.PublicKey(kid)
		}, jwt.WithLeeway(5*time.Second))
	if err != nil {
		// Key-resolution failures surface through the keyfunc wrapped in the
		// parse error. They are OUR fault, not the token's, so they must not be
		// flattened into ErrTokenInvalid — that would answer 401 and tell the
		// client to re-authenticate against an authn that is simply unreachable.
		if errors.Is(err, ErrKeyUnavailable) {
			return KrateoClaims{}, err
		}
		if !errors.Is(err, jwt.ErrTokenExpired) {
			return KrateoClaims{}, ErrTokenInvalid
		}
		return KrateoClaims{}, ErrTokenExpired
	}

	claims, ok := tok.Claims.(*KrateoClaims)
	if !ok {
		return KrateoClaims{}, ErrTokenInvalid
	}

	// A signed token whose actor chain is malformed names no one reliably:
	// refuse it rather than attribute its requests to an empty actor.
	if !validActor(claims.Act) {
		return KrateoClaims{}, ErrTokenInvalid
	}

	return *claims, nil
}
