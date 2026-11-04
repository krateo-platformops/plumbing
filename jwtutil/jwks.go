package jwtutil

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/krateo-platformops/plumbing/dialguard"
	"io"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const DefaultJWKSPath = "/.well-known/jwks.json"

const (
	// defaultJWKSCacheTTL is how long a successfully fetched key set is trusted
	// before it is refreshed.
	defaultJWKSCacheTTL = 5 * time.Minute

	// defaultJWKSMinRefreshInterval floors the gap between two fetch attempts.
	defaultJWKSMinRefreshInterval = 30 * time.Second

	// defaultJWKSRequestTimeout bounds a single fetch.
	defaultJWKSRequestTimeout = 5 * time.Second

	maxJWKSResponseBytes = 1 << 20
)

// ErrKeyUnavailable reports that the key a token was signed with could not be
// resolved — the key set was unreachable, malformed, or carried no key for the
// token's "kid". It is distinct from ErrTokenInvalid on purpose: the token may
// be perfectly valid and the fault ours, so callers should answer 503 rather
// than 401 and let the client retry.
var ErrKeyUnavailable = errKeyUnavailable{}

// errKeyUnavailable carries the underlying cause while still matching
// errors.Is(err, ErrKeyUnavailable), so logs keep the detail ("connection
// refused", "no key for kid X") that a bare sentinel would discard.
type errKeyUnavailable struct{ cause error }

func (e errKeyUnavailable) Error() string {
	if e.cause == nil {
		return "token signing key unavailable"
	}
	return "token signing key unavailable: " + e.cause.Error()
}

func (e errKeyUnavailable) Unwrap() error { return e.cause }

// Is matches any errKeyUnavailable regardless of cause, so the exported
// sentinel works as a target for every wrapped instance.
func (e errKeyUnavailable) Is(target error) bool {
	_, ok := target.(errKeyUnavailable)
	return ok
}

func keyUnavailable(format string, args ...any) error {
	return errKeyUnavailable{cause: fmt.Errorf(format, args...)}
}

// KeySource resolves the RSA public key that a token's "kid" header names.
// Implementations must be safe for concurrent use.
type KeySource interface {
	PublicKey(kid string) (*rsa.PublicKey, error)
}

// StaticKeySource serves one fixed key for every kid. It is the adapter that
// keeps the plain Validate(publicKey, bearer) form working, and is the simplest
// stand-in in tests.
type StaticKeySource struct {
	key *rsa.PublicKey
}

// NewStaticKeySource returns a KeySource that always yields key.
func NewStaticKeySource(key *rsa.PublicKey) *StaticKeySource {
	return &StaticKeySource{key: key}
}

// PublicKey ignores kid and returns the configured key.
func (s *StaticKeySource) PublicKey(string) (*rsa.PublicKey, error) {
	if s == nil || s.key == nil {
		return nil, keyUnavailable("no public key configured")
	}
	return s.key, nil
}

// JWKSKeySource resolves signing keys from a remote JWKS document (authn's
// /.well-known/jwks.json), caching the result.
//
// Nothing is fetched at construction: the first fetch happens on the first
// validation. A component wired to an authn that is not up yet therefore still
// starts and serves its unauthenticated routes, and recovers on its own once
// authn answers — no startup ordering dependency, no crash loop.
//
// Cache behaviour:
//   - A key set younger than TTL answers from memory.
//   - Past TTL, or when a token names a kid the cache does not hold (key
//     rotation), a refetch is attempted — but never more often than
//     MinRefreshInterval, so an unknown kid cannot turn into a request-rate
//     stampede against authn.
//   - If a refetch fails but the cache still holds the requested kid, the
//     stale-but-known key is served. A brief authn outage does not invalidate
//     tokens that were already verifiable.
type JWKSKeySource struct {
	url                string
	httpClient         *http.Client
	ttl                time.Duration
	minRefreshInterval time.Duration

	// now is swappable so tests can drive cache expiry without sleeping.
	now func() time.Time

	// mu guards every field below AND serialises fetches. Holding it across the
	// HTTP round trip is deliberate: it single-flights concurrent misses into
	// one request. The wait is bounded by the client's timeout.
	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

// JWKSOption customises a JWKSKeySource.
type JWKSOption func(*JWKSKeySource)

// WithJWKSCacheTTL sets how long a fetched key set is served before refresh.
// Non-positive values are ignored.
func WithJWKSCacheTTL(ttl time.Duration) JWKSOption {
	return func(s *JWKSKeySource) {
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

// WithJWKSMinRefreshInterval sets the floor between two fetch attempts.
// Non-positive values are ignored.
func WithJWKSMinRefreshInterval(d time.Duration) JWKSOption {
	return func(s *JWKSKeySource) {
		if d > 0 {
			s.minRefreshInterval = d
		}
	}
}

// WithJWKSHTTPClient replaces the HTTP client used for fetches. The client
// should set a timeout; it bounds how long a validation can block.
func WithJWKSHTTPClient(c *http.Client) JWKSOption {
	return func(s *JWKSKeySource) {
		if c != nil {
			s.httpClient = c
		}
	}
}

// WithJWKSRequestTimeout sets the per-fetch timeout on the default client.
// Ignored when WithJWKSHTTPClient supplied a client of its own.
func WithJWKSRequestTimeout(d time.Duration) JWKSOption {
	return func(s *JWKSKeySource) {
		if d > 0 {
			s.httpClient.Timeout = d
		}
	}
}

// NewJWKSKeySource returns a KeySource that reads authn's key set from url.
// url must be the full JWKS document URL; see JWKSURL for building it from a
// base URL.
func NewJWKSKeySource(url string, opts ...JWKSOption) *JWKSKeySource {
	s := &JWKSKeySource{
		url: url,
		// dialguard.Client rather than &http.Client{}: a bare client uses http.DefaultTransport,
		// whose dialer has no Control hook, so this dial bypassed the destination policy entirely
		// (core-provider#306 / snowplow#1131).
		httpClient:         dialguard.Client(defaultJWKSRequestTimeout),
		ttl:                defaultJWKSCacheTTL,
		minRefreshInterval: defaultJWKSMinRefreshInterval,
		now:                time.Now,
		keys:               map[string]*rsa.PublicKey{},
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// JWKSURL joins a base URL (e.g. authn's service URL) with DefaultJWKSPath.
// A base that already points at a .json document is returned unchanged, so
// callers may configure either form.
func JWKSURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, ".json") {
		return base
	}
	return strings.TrimSuffix(base, "/") + DefaultJWKSPath
}

// PublicKey returns the key named by kid, fetching or refreshing the key set as
// needed. An empty kid resolves to the only key in the set when the set holds
// exactly one — tokens minted before "kid" headers existed stay verifiable.
func (s *JWKSKeySource) PublicKey(kid string) (*rsa.PublicKey, error) {
	if s == nil {
		return nil, keyUnavailable("no JWKS key source configured")
	}
	if s.url == "" {
		return nil, keyUnavailable("no JWKS URL configured")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cached, found := s.lookupLocked(kid)
	if found && !s.fetchedAt.IsZero() && s.now().Sub(s.fetchedAt) < s.ttl {
		return cached, nil
	}

	// A refetch is wanted: the cache is empty, past its TTL, or missing this
	// kid. Honour the refresh floor so misses cannot become a request flood.
	if s.lastAttempt.IsZero() || s.now().Sub(s.lastAttempt) >= s.minRefreshInterval {
		if err := s.refreshLocked(); err != nil {
			// Prefer a stale-but-known key over failing the request.
			if found {
				return cached, nil
			}
			return nil, err
		}
		if key, ok := s.lookupLocked(kid); ok {
			return key, nil
		}
	} else if found {
		// Throttled out of refreshing, but the cache can still answer.
		return cached, nil
	}

	if kid == "" {
		return nil, keyUnavailable("JWKS at %s holds %d usable keys; token has no kid to select one",
			s.url, len(s.keys))
	}
	return nil, keyUnavailable("JWKS at %s holds no key for kid %q", s.url, kid)
}

// lookupLocked resolves kid against the cache. Callers must hold s.mu.
func (s *JWKSKeySource) lookupLocked(kid string) (*rsa.PublicKey, bool) {
	if kid != "" {
		key, ok := s.keys[kid]
		return key, ok
	}

	// No kid: unambiguous only when the set holds a single key.
	if len(s.keys) != 1 {
		return nil, false
	}
	for _, key := range s.keys {
		return key, true
	}
	return nil, false
}

// refreshLocked fetches and replaces the cached key set. The cache is left
// untouched on failure so a stale set remains available. Callers must hold s.mu.
func (s *JWKSKeySource) refreshLocked() error {
	s.lastAttempt = s.now()

	keys, err := s.fetch()
	if err != nil {
		return err
	}

	s.keys = keys
	s.fetchedAt = s.now()
	return nil
}

// fetch retrieves and decodes the key set.
func (s *JWKSKeySource) fetch() (map[string]*rsa.PublicKey, error) {
	res, err := s.httpClient.Get(s.url)
	if err != nil {
		return nil, keyUnavailable("fetching JWKS from %s: %w", s.url, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, keyUnavailable("fetching JWKS from %s: unexpected status %s", s.url, res.Status)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxJWKSResponseBytes))
	if err != nil {
		return nil, keyUnavailable("reading JWKS from %s: %w", s.url, err)
	}

	var doc jwksDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, keyUnavailable("decoding JWKS from %s: %w", s.url, err)
	}

	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		key, ok := k.rsaPublicKey()
		if !ok {
			// Skip anything not an RS256-capable RSA key (EC keys, encryption
			// keys, malformed entries) rather than failing the whole set.
			continue
		}
		keys[k.Kid] = key
	}

	if len(keys) == 0 {
		return nil, keyUnavailable("JWKS from %s contains no usable RSA keys", s.url)
	}

	return keys, nil
}

type jwksDocument struct {
	Keys []jwksKey `json:"keys"`
}

// jwksKey is the subset of RFC 7517 needed to verify an RS256 signature.
type jwksKey struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// rsaPublicKey rebuilds the RSA public key from the JWK's modulus/exponent.
// It reports false for keys that cannot verify an RS256 signature.
func (k jwksKey) rsaPublicKey() (*rsa.PublicKey, bool) {
	if k.Kty != "RSA" {
		return nil, false
	}
	// "use" and "alg" are optional; reject only an explicit mismatch.
	if k.Use != "" && k.Use != "sig" {
		return nil, false
	}
	if k.Alg != "" && k.Alg != "RS256" {
		return nil, false
	}

	nBytes, err := decodeBase64URL(k.N)
	if err != nil || len(nBytes) == 0 {
		return nil, false
	}
	eBytes, err := decodeBase64URL(k.E)
	if err != nil || len(eBytes) == 0 {
		return nil, false
	}

	// Bound the exponent so the big.Int → int conversion is safe on every
	// platform. Real exponents are 65537.
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() <= 0 || e.Int64() > math.MaxInt32 {
		return nil, false
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e.Int64()),
	}, true
}

// decodeBase64URL decodes a JWK field. RFC 7517 mandates unpadded base64url,
// but padded input is accepted too since some issuers emit it.
func decodeBase64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
