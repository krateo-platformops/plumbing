package jwtutil

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rotatingJWKS is an authn whose published key set can be swapped, or made to
// fail, between requests.
type rotatingJWKS struct {
	mu   sync.Mutex
	body string
	fail bool
}

func (r *rotatingJWKS) set(t *testing.T, keys map[string]*rsa.PublicKey) {
	t.Helper()
	body := jwksBody(t, keys)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.body, r.fail = body, false
}

func (r *rotatingJWKS) down() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = true
}

func (r *rotatingJWKS) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, r.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return key
}

// A key retired to revoke the tokens it signed: once the validator's key set is
// refreshed without it, a token it signed is invalid (401, sign in again), not
// a key we failed to get (503, retry). Until that refresh the cached key still
// verifies, so a revocation takes effect within the cache TTL.
func TestRetiredKeyIsAnInvalidToken(t *testing.T) {
	oldKey, newKey := rsaKey(t), rsaKey(t)
	authn := &rotatingJWKS{}
	authn.set(t, map[string]*rsa.PublicKey{"key-1": &oldKey.PublicKey})

	clock := time.Now()
	src := NewJWKSKeySource(JWKSURL(authn.serve(t).URL), WithJWKSCacheTTL(5*time.Minute))
	src.now = func() time.Time { return clock }

	leaked := newToken(t, oldKey, "key-1", time.Hour)
	_, err := ValidateWithKeySource(src, leaked)
	require.NoError(t, err)

	// authn rolls to key-2 and stops publishing key-1.
	authn.set(t, map[string]*rsa.PublicKey{"key-2": &newKey.PublicKey})

	clock = clock.Add(4 * time.Minute)
	_, err = ValidateWithKeySource(src, leaked)
	require.NoError(t, err, "within the TTL the cached key-1 still verifies")

	clock = clock.Add(2 * time.Minute)
	_, err = ValidateWithKeySource(src, leaked)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTokenInvalid)
	assert.ErrorIs(t, err, ErrKeyNotPublished)
	assert.NotErrorIs(t, err, ErrKeyUnavailable, "a retired key must not read as 503")
	assert.Contains(t, err.Error(), `kid "key-1"`)

	// Later lookups, throttled or not, keep answering 401.
	for range 3 {
		_, err = ValidateWithKeySource(src, leaked)
		assert.ErrorIs(t, err, ErrKeyNotPublished)
	}

	// Tokens signed with the new key verify.
	_, err = ValidateWithKeySource(src, newToken(t, newKey, "key-2", time.Hour))
	require.NoError(t, err)
}

// The rotation moment: a token signed with a new key reaches a validator whose
// key set was fetched before that key existed, and the refresh floor stops a
// refetch. The token may be perfectly valid, so it stays 503 (retry), and the
// next allowed refetch verifies it.
func TestNewKeyTheCachedSetPredatesIsUnavailable(t *testing.T) {
	oldKey, newKey := rsaKey(t), rsaKey(t)
	authn := &rotatingJWKS{}
	authn.set(t, map[string]*rsa.PublicKey{"key-1": &oldKey.PublicKey})

	// The set is fetched a minute before the new key signs anything.
	clock := time.Now().Add(-time.Minute)
	src := NewJWKSKeySource(JWKSURL(authn.serve(t).URL), WithJWKSMinRefreshInterval(30*time.Second))
	src.now = func() time.Time { return clock }
	_, err := src.PublicKey("key-1")
	require.NoError(t, err)

	authn.set(t, map[string]*rsa.PublicKey{"key-2": &newKey.PublicKey})
	fresh := newToken(t, newKey, "key-2", time.Hour)

	clock = clock.Add(10 * time.Second) // inside the refresh floor
	_, err = ValidateWithKeySource(src, fresh)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyUnavailable)
	assert.NotErrorIs(t, err, ErrTokenInvalid, "a key the cached set predates must not read as 401")

	clock = clock.Add(30 * time.Second) // past the floor: refetch finds key-2
	_, err = ValidateWithKeySource(src, fresh)
	require.NoError(t, err)
}

// When no key set was ever fetched, or the fetch succeeds with no usable key
// (a misconfigured issuer), nothing is known about the token's key: 503, never
// 401. A 401 there would send every client round a sign-in loop against a
// broken JWKS endpoint.
func TestNoKeySetIsUnavailableNotInvalid(t *testing.T) {
	key := rsaKey(t)
	token := newToken(t, key, "key-1", time.Hour)

	t.Run("fetch fails", func(t *testing.T) {
		authn := &rotatingJWKS{}
		authn.down()
		src := NewJWKSKeySource(JWKSURL(authn.serve(t).URL))
		_, err := ValidateWithKeySource(src, token)
		assert.ErrorIs(t, err, ErrKeyUnavailable)
		assert.NotErrorIs(t, err, ErrTokenInvalid)
		assert.NotErrorIs(t, err, ErrKeyNotPublished)
	})

	t.Run("fetch succeeds with an empty set", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"keys":[]}`)
		}))
		t.Cleanup(srv.Close)
		src := NewJWKSKeySource(JWKSURL(srv.URL))
		_, err := ValidateWithKeySource(src, token)
		assert.ErrorIs(t, err, ErrKeyUnavailable)
		assert.NotErrorIs(t, err, ErrTokenInvalid)
		assert.NotErrorIs(t, err, ErrKeyNotPublished)
	})
}

// While the latest refetch fails, an absent kid is 503 even when an earlier
// set lacked it: authn may be down in the middle of a rotation. Once a refetch
// succeeds, the retired key is 401 again.
func TestAbsentKidIsUnavailableWhileJWKSIsDown(t *testing.T) {
	oldKey, newKey := rsaKey(t), rsaKey(t)
	leaked := newToken(t, oldKey, "key-1", time.Hour)

	authn := &rotatingJWKS{}
	authn.set(t, map[string]*rsa.PublicKey{"key-2": &newKey.PublicKey})

	clock := time.Now().Add(time.Minute)
	src := NewJWKSKeySource(JWKSURL(authn.serve(t).URL), WithJWKSMinRefreshInterval(30*time.Second))
	src.now = func() time.Time { return clock }
	_, err := ValidateWithKeySource(src, leaked)
	require.ErrorIs(t, err, ErrKeyNotPublished)

	authn.down()
	clock = clock.Add(time.Minute) // past the floor: the refetch fails
	_, err = ValidateWithKeySource(src, leaked)
	assert.ErrorIs(t, err, ErrKeyUnavailable)
	assert.NotErrorIs(t, err, ErrKeyNotPublished)

	clock = clock.Add(10 * time.Second) // throttled after the failure
	_, err = ValidateWithKeySource(src, leaked)
	assert.ErrorIs(t, err, ErrKeyUnavailable)
	assert.NotErrorIs(t, err, ErrKeyNotPublished)

	authn.set(t, map[string]*rsa.PublicKey{"key-2": &newKey.PublicKey})
	clock = clock.Add(time.Minute)
	_, err = ValidateWithKeySource(src, leaked)
	assert.ErrorIs(t, err, ErrKeyNotPublished)
}

// A token without an issue time can't be placed before or after the cached set,
// so an absent kid stays 503.
func TestAbsentKidWithoutIssueTimeIsUnavailable(t *testing.T) {
	oldKey, newKey := rsaKey(t), rsaKey(t)
	authn := &rotatingJWKS{}
	authn.set(t, map[string]*rsa.PublicKey{"key-2": &newKey.PublicKey})

	clock := time.Now().Add(time.Minute)
	src := NewJWKSKeySource(JWKSURL(authn.serve(t).URL))
	src.now = func() time.Time { return clock }

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, KrateoClaims{
		UserInfo:         UserInfo{Username: "alice"},
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	})
	tok.Header["kid"] = "key-1"
	signed, err := tok.SignedString(oldKey)
	require.NoError(t, err)

	_, err = ValidateWithKeySource(src, signed)
	assert.ErrorIs(t, err, ErrKeyUnavailable)
	assert.NotErrorIs(t, err, ErrKeyNotPublished)
}
