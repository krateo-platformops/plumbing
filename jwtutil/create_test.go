package jwtutil_test

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateToken(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	const keyID = "test-key-id"

	tests := []struct {
		name     string
		username string
		groups   []string
		duration time.Duration
	}{
		{
			name:     "with groups",
			username: "alice",
			groups:   []string{"admin", "dev"},
			duration: time.Minute * 30,
		},
		{
			name:     "without groups",
			username: "bob",
			groups:   []string{},
			duration: time.Minute * 15,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := jwtutil.CreateTokenOptions{
				Username:   tc.username,
				Groups:     tc.groups,
				Duration:   tc.duration,
				KeyID:      keyID,
				PrivateKey: privateKey,
			}

			tokenStr, err := jwtutil.CreateToken(opts)
			assert.NoError(t, err)
			assert.NotEmpty(t, tokenStr)

			// Parse and validate the token using the public key.
			token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (any, error) {
				return &privateKey.PublicKey, nil
			})
			assert.NoError(t, err)
			assert.True(t, token.Valid)

			// The token must be signed with RS256 and carry the kid header.
			assert.Equal(t, jwt.SigningMethodRS256.Alg(), token.Method.Alg())
			assert.Equal(t, keyID, token.Header["kid"])

			claims, ok := token.Claims.(jwt.MapClaims)
			assert.True(t, ok)

			assert.Equal(t, tc.username, claims["sub"])
			assert.Equal(t, tc.username, claims["username"])
			assert.ElementsMatch(t, tc.groups, claims["groups"])
		})
	}
}

func TestCreateTokenValidation(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tests := []struct {
		name string
		opts jwtutil.CreateTokenOptions
	}{
		{
			name: "nil private key",
			opts: jwtutil.CreateTokenOptions{Username: "alice", KeyID: "kid"},
		},
		{
			name: "empty key ID",
			opts: jwtutil.CreateTokenOptions{Username: "alice", PrivateKey: privateKey},
		},
		{
			name: "empty username",
			opts: jwtutil.CreateTokenOptions{KeyID: "kid", PrivateKey: privateKey},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jwtutil.CreateToken(tc.opts)
			assert.Error(t, err)
		})
	}
}

// A token carries "aud" only when asked: one audience as a string, several as
// a list, none at all by default. The package's validators, which don't check
// it, accept a token carrying one exactly as they accept one without.
func TestCreateTokenAudience(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	audOf := func(t *testing.T, aud []string) (any, bool) {
		t.Helper()
		tokenStr, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{Username: "alice", Duration: time.Minute,
			KeyID: "k", PrivateKey: privateKey, Audience: aud})
		require.NoError(t, err)
		info, err := jwtutil.Validate(&privateKey.PublicKey, tokenStr)
		require.NoError(t, err, "a token with or without aud validates alike")
		assert.Equal(t, "alice", info.Username)
		claims := jwt.MapClaims{}
		_, _, err = jwt.NewParser().ParseUnverified(tokenStr, claims)
		require.NoError(t, err)
		v, ok := claims["aud"]
		return v, ok
	}

	_, ok := audOf(t, nil)
	assert.False(t, ok, "no aud unless asked")
	v, ok := audOf(t, []string{"agentiko"})
	require.True(t, ok)
	assert.Equal(t, "agentiko", v, "one audience is a bare string on the wire (RFC 7519 §4.1.3)")
	got, err := jwt.MapClaims{"aud": v}.GetAudience()
	require.NoError(t, err)
	assert.Equal(t, jwt.ClaimStrings{"agentiko"}, got)
	v, ok = audOf(t, []string{"agentiko", "snowplow"})
	require.True(t, ok)
	assert.Equal(t, []any{"agentiko", "snowplow"}, v, "several are a list")
	got, err = jwt.MapClaims{"aud": v}.GetAudience()
	require.NoError(t, err)
	assert.Equal(t, jwt.ClaimStrings{"agentiko", "snowplow"}, got)

	for _, aud := range [][]string{{""}, {"agentiko", ""}} {
		_, err = jwtutil.CreateToken(jwtutil.CreateTokenOptions{Username: "alice", Duration: time.Minute,
			KeyID: "k", PrivateKey: privateKey, Audience: aud})
		assert.ErrorContains(t, err, "audience cannot be empty", "%q", aud)
	}
}
