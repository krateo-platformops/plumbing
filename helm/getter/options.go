package getter

import (
	"log/slog"
	"time"

	"github.com/krateo-platformops/plumbing/helm/getter/cache"
)

type GetOptions struct {
	URI                   string
	Version               string
	Repo                  string
	InsecureSkipVerifyTLS bool
	Username              string
	Password              string
	PassCredentialsAll    bool
	Timeout               time.Duration
	Logging               *slog.Logger
	Cache                 *cache.DiskCache
	// Verify makes Get check the chart's provenance (its .prov) against Keyring before returning it
	// (plumbing#72). See WithVerify.
	Verify bool
	// Keyring is the path to the keyring (public keys) Verify checks the signature against.
	Keyring string
}

type Option func(*GetOptions) error

func WithLogger(l *slog.Logger) Option {
	return func(o *GetOptions) error {
		o.Logging = l
		return nil
	}
}

func WithVersion(v string) Option {
	return func(o *GetOptions) error {
		o.Version = v
		return nil
	}
}

func WithRepo(r string) Option {
	return func(o *GetOptions) error {
		o.Repo = r
		return nil
	}
}

func WithCredentials(username, password string) Option {
	return func(o *GetOptions) error {
		o.Username = username
		o.Password = password
		return nil
	}
}

func WithPassCredentialsAll(pass bool) Option {
	return func(o *GetOptions) error {
		o.PassCredentialsAll = pass
		return nil
	}
}

func WithInsecureSkipVerifyTLS(skip bool) Option {
	return func(o *GetOptions) error {
		o.InsecureSkipVerifyTLS = skip
		return nil
	}
}

func WithTimeout(t time.Duration) Option {
	return func(o *GetOptions) error {
		o.Timeout = t
		return nil
	}
}

func WithCache(c *cache.DiskCache) Option {
	return func(o *GetOptions) error {
		o.Cache = c
		return nil
	}
}

// WithVerify makes Get verify the chart's provenance file against the keyring at keyringPath, as
// `helm pull --verify --keyring` does (plumbing#72): the .prov must carry a signature by a key in
// the keyring and the chart's sha256 under the chart's file name. A chart that is unsigned, signed
// by another key, or altered is refused with ErrProvenance, never returned. An empty keyringPath is
// refused with ErrNoKeyring: verification without a keyring would verify nothing.
func WithVerify(keyringPath string) Option {
	return func(o *GetOptions) error {
		o.Verify = true
		o.Keyring = keyringPath
		return nil
	}
}
