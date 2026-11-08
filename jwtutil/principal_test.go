package jwtutil_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A token created without a principal has no principal claim, so it is what
// it was before the claim existed, and reads back as "".
func TestTokenWithoutPrincipalHasNoClaim(t *testing.T) {
	key := actKey(t)
	tok := actToken(t, key, nil)

	_, present := payload(t, tok)["principal"]
	assert.False(t, present)

	nfo, err := jwtutil.Validate(&key.PublicKey, tok)
	require.NoError(t, err)
	assert.Equal(t, jwtutil.Principal(""), nfo.Principal)
	assert.Equal(t, jwtutil.Principal(""), nfo.ActorPrincipal())
}

// Each of the four kinds is signed as its plain string and reads back as
// itself; on an ordinary token the actor's kind is the token's own.
func TestPrincipalRoundTrips(t *testing.T) {
	key := actKey(t)
	for _, p := range []jwtutil.Principal{
		jwtutil.PrincipalHuman, jwtutil.PrincipalAgent,
		jwtutil.PrincipalController, jwtutil.PrincipalService,
	} {
		t.Run(string(p), func(t *testing.T) {
			tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
				Username: "alice", Duration: time.Minute, KeyID: "k", PrivateKey: key, Principal: p,
			})
			require.NoError(t, err)
			assert.Equal(t, string(p), payload(t, tok)["principal"])

			nfo, err := jwtutil.Validate(&key.PublicKey, tok)
			require.NoError(t, err)
			assert.Equal(t, p, nfo.Principal)
			assert.Equal(t, p, nfo.ActorPrincipal())
		})
	}
}

// On a delegated token the subject stays human and each act link carries its
// own kind; ActorPrincipal is the outermost actor's.
func TestDelegatedTokenCarriesActorPrincipals(t *testing.T) {
	key := actKey(t)
	act := &jwtutil.Actor{Sub: "agent-b", Principal: jwtutil.PrincipalAgent,
		Act: &jwtutil.Actor{Sub: "svc-a", Principal: jwtutil.PrincipalService}}
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username: "alice", Duration: time.Minute, KeyID: "k", PrivateKey: key,
		Principal: jwtutil.PrincipalHuman, Act: act,
	})
	require.NoError(t, err)

	claims := payload(t, tok)
	assert.Equal(t, "human", claims["principal"])
	assert.Equal(t, map[string]any{
		"sub": "agent-b", "principal": "agent",
		"act": map[string]any{"sub": "svc-a", "principal": "service"},
	}, claims["act"])

	nfo, err := jwtutil.Validate(&key.PublicKey, tok)
	require.NoError(t, err)
	assert.Equal(t, jwtutil.PrincipalHuman, nfo.Principal)
	assert.Equal(t, act, nfo.Act)
	assert.Equal(t, jwtutil.PrincipalAgent, nfo.ActorPrincipal())
}

// An actor without a kind reads as "", even when the subject has one: the
// subject's kind never stands in for the actor's.
func TestActorPrincipalDoesNotFallBackToSubject(t *testing.T) {
	key := actKey(t)
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username: "alice", Duration: time.Minute, KeyID: "k", PrivateKey: key,
		Principal: jwtutil.PrincipalHuman, Act: &jwtutil.Actor{Sub: "agent-b"},
	})
	require.NoError(t, err)

	nfo, err := jwtutil.Validate(&key.PublicKey, tok)
	require.NoError(t, err)
	assert.Equal(t, jwtutil.Principal(""), nfo.ActorPrincipal())
}

// CreateToken refuses a kind outside the four, on the token or on any actor.
func TestCreateTokenRefusesUnknownPrincipal(t *testing.T) {
	for name, opts := range map[string]jwtutil.CreateTokenOptions{
		"on the token":    {Principal: "robot"},
		"on the actor":    {Act: &jwtutil.Actor{Sub: "agent-b", Principal: "robot"}},
		"on a deep actor": {Act: &jwtutil.Actor{Sub: "agent-b", Act: &jwtutil.Actor{Sub: "svc-a", Principal: "Human"}}},
	} {
		t.Run(name, func(t *testing.T) {
			opts.Username, opts.Duration, opts.KeyID, opts.PrivateKey = "alice", time.Minute, "k", actKey(t)
			_, err := jwtutil.CreateToken(opts)
			assert.Error(t, err)
		})
	}
}

// A signed token whose principal is unknown or not a string still validates,
// and the kind reads as "": a newer issuer's kind never breaks an older
// validator.
func TestValidateReadsUnknownPrincipalAsEmpty(t *testing.T) {
	key := actKey(t)
	for name, v := range map[string]any{
		"unknown string": "robot",
		"wrong case":     "Human",
		"number":         7,
		"object":         map[string]any{"kind": "human"},
		"null":           nil,
	} {
		t.Run(name, func(t *testing.T) {
			claims := jwt.MapClaims{
				"iss": "krateo.io", "sub": "alice", "username": "alice",
				"exp":       time.Now().Add(time.Minute).Unix(),
				"principal": v,
				"act":       map[string]any{"sub": "agent-b", "principal": v},
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			tok.Header["kid"] = "k"
			signed, err := tok.SignedString(key)
			require.NoError(t, err)

			nfo, err := jwtutil.Validate(&key.PublicKey, signed)
			require.NoError(t, err)
			assert.Equal(t, "alice", nfo.Username)
			assert.Equal(t, jwtutil.Principal(""), nfo.Principal)
			require.NotNil(t, nfo.Act)
			assert.Equal(t, "agent-b", nfo.Act.Sub)
			assert.Equal(t, jwtutil.Principal(""), nfo.ActorPrincipal())
		})
	}
}
