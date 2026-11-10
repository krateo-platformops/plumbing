package use

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signedAt signs a token for kid with key, issued a minute ago, as authn would
// have before a rotation.
func signedAt(t *testing.T, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwtutil.KrateoClaims{
		UserInfo: jwtutil.UserInfo{Username: "alice", Principal: jwtutil.PrincipalHuman},
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	})
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	require.NoError(t, err)
	return signed
}

func jwksServer(t *testing.T, status int, kid string, pub *rsa.PublicKey) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return jwtutil.JWKSURL(srv.URL)
}

// After authn retires a key, a token it signed is answered 401, so the client
// signs in again. When the key set can't be fetched, the answer stays 503, so
// the client retries rather than signing in against an authn that is down.
func TestUserConfigRetiredKeyIs401AndUnreachableIs503(t *testing.T) {
	oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	revoked := signedAt(t, oldKey, "key-1")

	for name, tc := range map[string]struct {
		jwks string
		want int
	}{
		"key-1 retired": {jwksServer(t, http.StatusOK, "key-2", &newKey.PublicKey), http.StatusUnauthorized},
		"JWKS down":     {jwksServer(t, http.StatusBadGateway, "key-2", &newKey.PublicKey), http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			h := UserConfig(jwtutil.NewJWKSKeySource(tc.jwks), "krateo-system")(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					t.Error("a token that doesn't verify must not reach the handler")
				}))
			req := httptest.NewRequest(http.MethodGet, "/call", nil)
			req.Header.Set("Authorization", "Bearer "+revoked)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}
}
