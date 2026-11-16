package getter

import (
	"errors"
)

var (
	// ErrNoHandler is returned when no suitable getter is found for the URL scheme.
	ErrNoHandler = errors.New("no handler found for url")

	// ErrFetchFailed is a generic error for download failures.
	ErrFetchFailed = errors.New("fetch failed")

	// ErrInvalidRepoRef is returned when a repo URL is malformed.
	ErrInvalidRepoRef = errors.New("invalid repo reference")

	// ErrProvenance is returned when Verify is set and the chart's provenance cannot be fetched or
	// does not verify: no .prov, a signature by no key in the keyring, or a chart whose sha256 is
	// not the signed one (plumbing#72).
	ErrProvenance = errors.New("chart provenance verification failed")

	// ErrNoKeyring is returned when Verify is set without a keyring.
	ErrNoKeyring = errors.New("chart provenance verification requested without a keyring")
)
