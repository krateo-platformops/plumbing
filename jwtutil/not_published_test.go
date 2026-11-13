package jwtutil

import (
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NotPublished on its own, as a caller verifying a token that is not authn's
// (an identity provider's access token, authn#155) uses it: only the token's
// issue time, against when the key set without its kid was fetched, picks
// 401 (retired or never published) or 503 (the set may predate the key).
func TestNotPublished(t *testing.T) {
	key := rsaKey(t)
	issuer := &rotatingJWKS{}
	issuer.set(t, map[string]*rsa.PublicKey{"key-1": &key.PublicKey})

	fetchedAt := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	src := NewJWKSKeySource(JWKSURL(issuer.serve(t).URL))
	src.now = func() time.Time { return fetchedAt }
	_, absent := src.PublicKey("key-9")
	require.Error(t, absent)
	require.ErrorIs(t, absent, ErrKeyUnavailable)

	for _, tc := range []struct {
		name      string
		issuedAt  time.Time
		published bool // still ErrKeyUnavailable
	}{
		{name: "issued well before the fetch: retired or never the issuer's", issuedAt: fetchedAt.Add(-time.Hour)},
		{name: "issued just past the leeway before the fetch", issuedAt: fetchedAt.Add(-validationLeeway - time.Second)},
		{name: "issued within the leeway before the fetch", issuedAt: fetchedAt.Add(-validationLeeway + time.Second), published: true},
		{name: "issued after the fetch: the set may predate the key", issuedAt: fetchedAt.Add(time.Minute), published: true},
		{name: "no issue time", issuedAt: time.Time{}, published: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := NotPublished(absent, tc.issuedAt)
			if tc.published {
				assert.Equal(t, absent, err, "the error passes through unchanged")
				return
			}
			assert.ErrorIs(t, err, ErrKeyNotPublished)
			assert.NotErrorIs(t, err, ErrKeyUnavailable, "a retired key must not read as 503")
			assert.Contains(t, err.Error(), `kid "key-9"`)
			assert.Contains(t, err.Error(), tc.issuedAt.Format(time.RFC3339))
		})
	}

	// Any other error passes through, whatever the issue time.
	other := errors.New("the issuer is down")
	assert.Equal(t, other, NotPublished(other, fetchedAt.Add(-time.Hour)))
	assert.NoError(t, NotPublished(nil, fetchedAt.Add(-time.Hour)))
}
