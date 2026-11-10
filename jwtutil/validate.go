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

// validationLeeway absorbs clock skew between authn and the validator, for
// expiry and for telling a retired key from a new one.
const validationLeeway = 5 * time.Second

// notPublished turns a kid absent from a successfully fetched key set into
// ErrKeyNotPublished when the token was issued before that set was fetched (by
// more than the leeway). authn publishes a key from the moment it signs with
// it, so that key was retired. A token issued later, or without an issue time,
// may carry a key the set predates: it stays ErrKeyUnavailable, and the next
// refetch finds the key. The issue time is read before the signature is
// checked, which is safe: the token fails either way, and it only picks 401 or
// 503. Every other error passes through unchanged.
func notPublished(err error, token *jwt.Token) error {
	var absent errKidAbsent
	if !errors.As(err, &absent) {
		return err
	}
	claims, ok := token.Claims.(*KrateoClaims)
	if !ok || claims.IssuedAt == nil {
		return err
	}
	if !claims.IssuedAt.Add(validationLeeway).Before(absent.asOf) {
		return err
	}
	return fmt.Errorf("%w: %s; the token was issued at %s",
		ErrKeyNotPublished, absent.Error(), claims.IssuedAt.UTC().Format(time.RFC3339))
}

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
// says nothing about the token. A token whose kid is absent from a key set
// fetched after the token was issued is ErrTokenInvalid and ErrKeyNotPublished:
// its key was retired, so it will never verify again.
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
			key, err := keys.PublicKey(kid)
			if err != nil {
				return nil, notPublished(err, token)
			}
			return key, nil
		}, jwt.WithLeeway(validationLeeway))
	if err != nil {
		// Key-resolution failures surface through the keyfunc wrapped in the
		// parse error. They are OUR fault, not the token's, so they must not be
		// flattened into ErrTokenInvalid — that would answer 401 and tell the
		// client to re-authenticate against an authn that is simply unreachable.
		if errors.Is(err, ErrKeyUnavailable) {
			return KrateoClaims{}, err
		}
		// A retired key is the token's fault: keep the detail and match
		// ErrTokenInvalid, so callers answer 401 and the client signs in again.
		if errors.Is(err, ErrKeyNotPublished) {
			return KrateoClaims{}, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
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
