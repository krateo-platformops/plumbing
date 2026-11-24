package jwtutil_test

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A recipient's answer for every shape an "aud" can take (RFC 7519 §4.1.3), with each option. The
// rows without an option are the behavior before options existed: "aud" isn't looked at.
func TestAudience(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keys := jwtutil.NewStaticKeySource(&key.PublicKey)
	absent := struct{}{}
	mk := func(aud any) string {
		c := jwt.MapClaims{"iss": "krateo.io", "sub": "alice", "username": "alice", "groups": []string{"devs"},
			"act": map[string]any{"sub": "agentgateway"}, "exp": time.Now().Add(time.Minute).Unix()}
		if aud != any(absent) {
			c["aud"] = aud
		}
		s, err := jwt.NewWithClaims(jwt.SigningMethodRS256, c).SignedString(key)
		require.NoError(t, err)
		return s
	}
	const mine, other = "snowplow.krateo-system", "config-refs.krateo-system"
	for _, tc := range []struct {
		name               string
		aud                any
		checked, requiring bool // accepted by WithAudience(mine), by WithRequiredAudience(mine)
	}{
		{"no aud, a sign-in token", absent, true, false},
		{"null", nil, true, false},
		{"mine, a string", mine, true, true},
		{"mine, a list", []string{mine}, true, true},
		{"mine among others", []string{other, mine}, true, true},
		{"another's, a string", other, false, false},
		{"another's, a list", []string{other}, false, false},
		{"an empty list names no one", []string{}, false, false},
		{"an empty string", "", false, false},
		{"a list of an empty string", []string{""}, false, false},
		{"a prefix of mine", "snowplow", false, false},
		{"mine, in another case", "Snowplow.krateo-system", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := mk(tc.aud)
			_, err := jwtutil.ValidateWithKeySource(keys, tok)
			assert.NoError(t, err, "without an option aud isn't looked at")
			for opt, want := range map[string]bool{"WithAudience": tc.checked, "WithRequiredAudience": tc.requiring} {
				o := jwtutil.WithAudience(mine)
				if opt == "WithRequiredAudience" {
					o = jwtutil.WithRequiredAudience(mine)
				}
				info, err := jwtutil.ValidateWithKeySource(keys, tok, o)
				claims, cerr := jwtutil.ValidateClaimsWithKeySource(keys, tok, o)
				// Accepted or refused, the token verified: both name who presented it.
				for _, who := range []jwtutil.UserInfo{info, claims.UserInfo} {
					assert.Equal(t, "alice", who.Username, opt)
					assert.Equal(t, "agentgateway", who.Actor(), opt)
				}
				if want {
					assert.NoError(t, err, opt)
					assert.NoError(t, cerr, opt)
					continue
				}
				for _, e := range []error{err, cerr} {
					assert.ErrorIs(t, e, jwtutil.ErrAudienceMismatch, opt)
					assert.ErrorIs(t, e, jwtutil.ErrTokenInvalid, "a caller that doesn't know the mismatch answers 401")
					assert.False(t, errors.Is(e, jwtutil.ErrTokenExpired) || errors.Is(e, jwtutil.ErrKeyUnavailable), opt)
				}
			}
		})
	}
	t.Run("several names", func(t *testing.T) {
		for aud, want := range map[string]bool{mine: true, other: true, "third": false} {
			_, err := jwtutil.ValidateWithKeySource(keys, mk(aud), jwtutil.WithAudience(mine, other))
			assert.Equal(t, want, err == nil, aud)
		}
	})
	t.Run("no name is no one", func(t *testing.T) {
		for _, o := range []jwtutil.ValidateOption{jwtutil.WithAudience(), jwtutil.WithAudience(""), jwtutil.WithRequiredAudience("")} {
			_, err := jwtutil.ValidateWithKeySource(keys, mk([]string{""}), o)
			assert.ErrorIs(t, err, jwtutil.ErrAudienceMismatch)
		}
	})
	t.Run("the last option wins", func(t *testing.T) {
		_, err := jwtutil.ValidateWithKeySource(keys, mk(absent), jwtutil.WithRequiredAudience(mine), jwtutil.WithAudience(mine))
		assert.NoError(t, err)
	})
}

// A token that doesn't verify is refused for that, and names no one, whatever its audience.
func TestAudienceOfATokenThatDoesNotVerify(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	mk := func(d time.Duration, act *jwtutil.Actor) string {
		tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{Username: "alice", Duration: d, KeyID: "k", PrivateKey: key,
			Audience: []string{"elsewhere"}, Act: act})
		require.NoError(t, err)
		return tok
	}
	badAct, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"username": "alice", "aud": "elsewhere",
		"act": map[string]any{"sub": ""}, "exp": time.Now().Add(time.Minute).Unix()}).SignedString(key)
	require.NoError(t, err)
	good := jwtutil.NewStaticKeySource(&key.PublicKey)
	for name, tc := range map[string]struct {
		keys jwtutil.KeySource
		tok  string
		want error
	}{
		"expired":           {good, mk(-time.Minute, nil), jwtutil.ErrTokenExpired},
		"another key's":     {jwtutil.NewStaticKeySource(&other.PublicKey), mk(time.Minute, nil), jwtutil.ErrTokenInvalid},
		"a malformed actor": {good, badAct, jwtutil.ErrTokenInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			info, err := jwtutil.ValidateWithKeySource(tc.keys, tc.tok, jwtutil.WithAudience("mine"))
			assert.ErrorIs(t, err, tc.want)
			assert.False(t, errors.Is(err, jwtutil.ErrAudienceMismatch))
			assert.Equal(t, jwtutil.UserInfo{}, info)
		})
	}
	// Control: the same audience, on a token that verifies, is the mismatch.
	_, err = jwtutil.ValidateWithKeySource(good, mk(time.Minute, nil), jwtutil.WithAudience("mine"))
	assert.ErrorIs(t, err, jwtutil.ErrAudienceMismatch)
}
