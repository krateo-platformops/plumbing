package jwtutil_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func actKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return k
}

func actToken(t *testing.T, key *rsa.PrivateKey, act *jwtutil.Actor) string {
	t.Helper()
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username: "alice", Groups: []string{"devs"}, Duration: time.Minute,
		KeyID: "k", PrivateKey: key, Act: act,
	})
	require.NoError(t, err)
	return tok
}

// payload decodes a token's claims without verifying it.
func payload(t *testing.T, tok string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// An ordinary token carries no act claim at all, so existing validators see
// exactly what they saw before.
func TestTokenWithoutActorHasNoActClaim(t *testing.T) {
	key := actKey(t)
	tok := actToken(t, key, nil)

	_, present := payload(t, tok)["act"]
	assert.False(t, present)

	nfo, err := jwtutil.Validate(&key.PublicKey, tok)
	require.NoError(t, err)
	assert.Nil(t, nfo.Act)
	assert.Equal(t, "alice", nfo.Actor())
	assert.Equal(t, []string{"alice"}, nfo.DelegationChain())
}

// A delegated token keeps the subject and adds RFC 8693's act, nested for a
// chain, and Validate returns it as signed.
func TestDelegatedTokenRoundTrips(t *testing.T) {
	key := actKey(t)
	act := &jwtutil.Actor{Sub: "agent-b", Act: &jwtutil.Actor{Sub: "agent-a"}}
	tok := actToken(t, key, act)

	claims := payload(t, tok)
	assert.Equal(t, "alice", claims["sub"])
	assert.Equal(t, map[string]any{"sub": "agent-b", "act": map[string]any{"sub": "agent-a"}}, claims["act"])

	nfo, err := jwtutil.Validate(&key.PublicKey, tok)
	require.NoError(t, err)
	assert.Equal(t, "alice", nfo.Username)
	assert.Equal(t, []string{"devs"}, nfo.Groups)
	assert.Equal(t, act, nfo.Act)
	assert.Equal(t, "agent-b", nfo.Actor())
	assert.Equal(t, []string{"agent-b", "agent-a", "alice"}, nfo.DelegationChain())
}

// CreateToken refuses a chain with an unnamed actor at any level, or one
// deeper than MaxActorDepth.
func TestCreateTokenRefusesMalformedActor(t *testing.T) {
	deep := &jwtutil.Actor{Sub: "a0"}
	for i := 1; i <= jwtutil.MaxActorDepth; i++ {
		deep = &jwtutil.Actor{Sub: "a", Act: deep}
	}
	for name, act := range map[string]*jwtutil.Actor{
		"empty outer actor":  {Sub: ""},
		"empty nested actor": {Sub: "agent-b", Act: &jwtutil.Actor{}},
		"too deep":           deep,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
				Username: "alice", Duration: time.Minute, KeyID: "k", PrivateKey: actKey(t), Act: act,
			})
			assert.Error(t, err)
		})
	}
}

// A signed token whose act chain is malformed is invalid: Validate never
// attributes a request to an empty actor.
func TestValidateRefusesMalformedActor(t *testing.T) {
	key := actKey(t)
	claims := jwt.MapClaims{
		"iss": "krateo.io", "sub": "alice", "username": "alice",
		"exp": time.Now().Add(time.Minute).Unix(),
		"act": map[string]any{"sub": "agent-b", "act": map[string]any{"sub": ""}},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k"
	signed, err := tok.SignedString(key)
	require.NoError(t, err)

	_, err = jwtutil.Validate(&key.PublicKey, signed)
	assert.ErrorIs(t, err, jwtutil.ErrTokenInvalid)
}
