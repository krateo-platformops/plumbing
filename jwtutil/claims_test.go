package jwtutil_test

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ValidateClaimsWithKeySource returns the verified registered claims too, so
// a caller can act on the expiry without parsing the token unverified.
func TestValidateClaimsReturnsVerifiedExpiry(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	act := &jwtutil.Actor{Sub: "agent"}
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username: "alice", Groups: []string{"devs"}, Duration: 10 * time.Minute,
		KeyID: "k", PrivateKey: key, Act: act,
	})
	require.NoError(t, err)

	claims, err := jwtutil.ValidateClaimsWithKeySource(jwtutil.NewStaticKeySource(&key.PublicKey), tok)
	require.NoError(t, err)
	assert.Equal(t, "alice", claims.Username)
	assert.Equal(t, []string{"devs"}, claims.Groups)
	assert.Equal(t, act, claims.Act)
	require.NotNil(t, claims.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(10*time.Minute), claims.ExpiresAt.Time, 5*time.Second)
}

// It refuses exactly what ValidateWithKeySource refuses, with the same errors.
func TestValidateClaimsRefusesLikeValidate(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	mk := func(d time.Duration) string {
		tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{Username: "alice", Duration: d, KeyID: "k", PrivateKey: key})
		require.NoError(t, err)
		return tok
	}
	keys := jwtutil.NewStaticKeySource(&key.PublicKey)

	_, err = jwtutil.ValidateClaimsWithKeySource(keys, mk(-time.Minute))
	assert.ErrorIs(t, err, jwtutil.ErrTokenExpired)

	_, err = jwtutil.ValidateClaimsWithKeySource(jwtutil.NewStaticKeySource(&other.PublicKey), mk(time.Minute))
	assert.ErrorIs(t, err, jwtutil.ErrTokenInvalid)

	_, err = jwtutil.ValidateClaimsWithKeySource(nil, mk(time.Minute))
	assert.Error(t, err)
}
