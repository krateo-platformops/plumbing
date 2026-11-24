package jwtutil

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrTokenExpired = errors.New("token is expired")
	ErrTokenInvalid = errors.New("token is invalid")
	// ErrAudienceMismatch: the token verified, and is meant for another
	// recipient (WithAudience, WithRequiredAudience). It also matches
	// ErrTokenInvalid, so a caller that doesn't know it answers 401 as for any
	// token it refuses. Signing in again doesn't help: it is the same kind of
	// token again.
	ErrAudienceMismatch = errors.New("token is for another audience")
)

// ValidateOption changes what a validator accepts. The last of several wins.
type ValidateOption func(*validateOptions)

type validateOptions struct {
	check, required bool
	names           []string
}

// WithAudience makes the validator a recipient called by any of names (RFC
// 7519 §4.1.3): a token whose "aud" lists none of them is refused with
// ErrAudienceMismatch. A token with no "aud", which every sign-in token is, is
// accepted. Without this option, or WithRequiredAudience, "aud" isn't looked at.
func WithAudience(names ...string) ValidateOption {
	return func(o *validateOptions) { *o = validateOptions{check: true, names: names} }
}

// WithRequiredAudience is WithAudience for a recipient that accepts only
// tokens issued for it: a token with no "aud" is refused too.
func WithRequiredAudience(names ...string) ValidateOption {
	return func(o *validateOptions) { *o = validateOptions{check: true, required: true, names: names} }
}

// admits reports whether a verified token with these audiences may be used
// here. present is whether the token carries "aud" at all: an empty list names
// no recipient, so it is for none.
func (o validateOptions) admits(aud []string, present bool) bool {
	if !o.check {
		return true
	}
	if !present {
		return !o.required
	}
	return slices.ContainsFunc(aud, func(a string) bool { return a != "" && slices.Contains(o.names, a) })
}

// hasAudience reports whether a token that verified carries an "aud" claim.
// The parsed claims can't say: an empty list reads as no claim there. A null
// is no claim.
func hasAudience(tok *jwt.Token) bool {
	parts := strings.Split(tok.Raw, ".")
	if len(parts) != 3 {
		return true
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return true
	}
	var c struct {
		Aud any `json:"aud"`
	}
	return json.Unmarshal(raw, &c) != nil || c.Aud != nil
}

// validationLeeway absorbs clock skew between authn and the validator, for
// expiry and for telling a retired key from a new one.
const validationLeeway = 5 * time.Second

// NotPublished turns a kid absent from a successfully fetched key set
// (JWKSKeySource.PublicKey's error) into ErrKeyNotPublished when the token was
// issued before that set was fetched, by more than the leeway: its issuer
// publishes a key from the moment it signs with it, so that key was retired,
// or never the issuer's, and no retry will make the token verify (401). A
// token issued later, or with no issue time (the zero time), may carry a key
// the set predates: the error stays ErrKeyUnavailable, and the next refetch
// finds the key (503). Every other error passes through unchanged.
//
// It is for any caller verifying a token against a JWKSKeySource, whatever
// the token's claims: authn's own tokens (ValidateWithKeySource), or an
// identity provider's (authn#155). issuedAt may be read before the signature
// is checked, which is safe: the token fails either way, and it only picks
// 401 or 503.
func NotPublished(err error, issuedAt time.Time) error {
	var absent errKidAbsent
	if !errors.As(err, &absent) || issuedAt.IsZero() {
		return err
	}
	if !issuedAt.Add(validationLeeway).Before(absent.asOf) {
		return err
	}
	return fmt.Errorf("%w: %s; the token was issued at %s",
		ErrKeyNotPublished, absent.Error(), issuedAt.UTC().Format(time.RFC3339))
}

// NotPublishedSince turns a kid absent from a key set fetched at or after
// receivedAt into ErrKeyNotPublished. receivedAt is when the caller received
// the token, read from the clock the key source reads (time.Now). A token can't
// be issued after it was received, and its issuer publishes a key from the
// moment it signs with it, so a set fetched since then that lacks the kid
// proves the key isn't published, whatever the token claims. Unlike
// NotPublished it needs no issue time, which an identity provider's token
// carries unsigned until it verifies (authn#155), and no leeway, since both
// times come from the caller's clock.
//
// Every other error passes through unchanged, including a kid absent from a
// set fetched before receivedAt (the source's refresh floor kept it from
// refetching). A caller may then still apply NotPublished, which decides from
// the issue time:
//
//	err = jwtutil.NotPublished(jwtutil.NotPublishedSince(err, receivedAt), issuedAt)
func NotPublishedSince(err error, receivedAt time.Time) error {
	var absent errKidAbsent
	if !errors.As(err, &absent) || receivedAt.IsZero() || absent.asOf.Before(receivedAt) {
		return err
	}
	return fmt.Errorf("%w: %s; the token was received at %s",
		ErrKeyNotPublished, absent.Error(), receivedAt.UTC().Format(time.RFC3339))
}

// notPublished is NotPublished for an authn token being parsed.
func notPublished(err error, token *jwt.Token) error {
	claims, ok := token.Claims.(*KrateoClaims)
	if !ok || claims.IssuedAt == nil {
		return err
	}
	return NotPublished(err, claims.IssuedAt.Time)
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
//
// WithAudience and WithRequiredAudience refuse a token meant for someone else.
// ErrAudienceMismatch is the one error returned with the token's identity,
// which verified: it says who presented the token, so the refusal can be
// recorded, and never that they may be served.
func ValidateWithKeySource(keys KeySource, bearer string, opts ...ValidateOption) (UserInfo, error) {
	claims, err := ValidateClaimsWithKeySource(keys, bearer, opts...)
	return claims.UserInfo, err
}

// ValidateClaimsWithKeySource verifies bearer exactly as ValidateWithKeySource
// does, with the same errors, and returns all of its verified claims: the
// identity and, unlike ValidateWithKeySource, the registered claims such as
// ExpiresAt. Use it when a decision depends on one of those, for example a
// token issued from this one that must not outlive it.
func ValidateClaimsWithKeySource(keys KeySource, bearer string, opts ...ValidateOption) (KrateoClaims, error) {
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

	var o validateOptions
	for _, opt := range opts {
		opt(&o)
	}
	if !o.admits(claims.Audience, hasAudience(tok)) {
		return *claims, fmt.Errorf("%w: %w", ErrTokenInvalid, ErrAudienceMismatch)
	}
	return *claims, nil
}
