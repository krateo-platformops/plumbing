package util

import (
	"context"
	"errors"
	"fmt"
	"github.com/krateo-platformops/plumbing/dialguard"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/krateo-platformops/plumbing/env"
	"golang.org/x/time/rate"
)

// NewRetryClient returns a RetryClient configured with retry and rate limiting
// settings sourced from environment variables.
//
// Environment variables (with defaults):
//
//   - CLIENT_MAX_RETRIES (int, default: 5)
//     Maximum number of retries before giving up.
//
//   - CLIENT_BASE_BACKOFF (duration, default: 500ms)
//     Initial backoff duration for retries. The backoff grows exponentially.
//
//   - CLIENT_MAX_BACKOFF (duration, default: 10s)
//     Maximum cap for the exponential backoff duration.
//
//   - CLIENT_QPS (float, default: 30.0)
//     Average sustained queries per second allowed by the rate limiter.
//
//   - CLIENT_BURST (int, default: 45)
//     Maximum burst of requests allowed before the limiter enforces QPS.
//
// The returned client retries failed requests (429 and 5xx) with exponential
// backoff and jitter, and enforces request rate limiting using a token bucket.
//
// Example usage:
//
//	cli := &http.Client{Timeout: 30 * time.Second}
//	retryCli := NewRetryClient(cli)
//	resp, err := retryCli.Do(req)
func NewRetryClient(cli *http.Client) *RetryClient {
	return &RetryClient{
		Client:      cli,
		MaxRetries:  env.Int("CLIENT_MAX_RETRIES", 5),
		BaseBackoff: env.Duration("CLIENT_BASE_BACKOFF", 500*time.Millisecond),
		MaxBackoff:  env.Duration("CLIENT_MAX_BACKOFF", 10*time.Second),
		Limiter: rate.NewLimiter(rate.Limit(
			env.Float64("CLIENT_QPS", 30.0)),
			env.Int("CLIENT_BURST", 45),
		),
	}
}

// RetryClient wraps an http.Client to add automatic retry on 429 and 5xx errors.
type RetryClient struct {
	Client      *http.Client
	MaxRetries  int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Limiter     *rate.Limiter // controls QPS and Burst
}

// Do executes the request with retry logic only for idempotent methods.
func (rc *RetryClient) Do(req *http.Request) (*http.Response, error) {
	if rc.Client == nil {
		// NOT http.DefaultClient: its transport is process-shared and its dialer has no Control
		// hook, so this fallback silently bypassed the destination policy
		// (core-provider#306 / snowplow#1131).
		rc.Client = dialguard.Client(0)
	}

	// enforce QPS/Burst limits
	if rc.Limiter != nil {
		if err := rc.Limiter.Wait(req.Context()); err != nil {
			return nil, err
		}
	}

	// Only retry idempotent HTTP methods
	if !isIdempotentMethod(req.Method) {
		return rc.Client.Do(req)
	}

	var lastErr error
	var resp *http.Response

	for attempt := 0; attempt <= rc.MaxRetries; attempt++ {
		// Clone the request to ensure Body can be reused
		clonedReq := req.Clone(req.Context())

		resp, lastErr = rc.Client.Do(clonedReq)
		if lastErr != nil {
			// Network error → retry
			if attempt < rc.MaxRetries {
				if err := sleepWithContext(req.Context(), backoff(attempt, rc.BaseBackoff, rc.MaxBackoff)); err != nil {
					return nil, err
				}
				continue
			}
			return nil, lastErr
		}

		// If response is successful → return immediately
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		// Handle 429 Too Many Requests with Retry-After
		if resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := parseRetryAfter(resp)
			resp.Body.Close()
			if attempt < rc.MaxRetries {
				if err := sleepWithContext(req.Context(), retryAfter); err != nil {
					return nil, err
				}
				continue
			}
			return resp, errors.New("too many retries after 429 Too Many Requests")
		}

		// Handle 5xx server errors
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			if attempt < rc.MaxRetries {
				if err := sleepWithContext(req.Context(), backoff(attempt, rc.BaseBackoff, rc.MaxBackoff)); err != nil {
					return nil, err
				}
				continue
			}
			return resp, fmt.Errorf("server error after %d retries: %d", rc.MaxRetries, resp.StatusCode)
		}
	}

	return resp, lastErr
}

// isIdempotentMethod returns true for HTTP methods that are safe to retry
func isIdempotentMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// backoff calculates exponential backoff with max cap and random jitter
func backoff(attempt int, base, max time.Duration) time.Duration {
	d := base * (1 << attempt) // exponential
	if d > max {
		d = max
	}
	// Add jitter: random between 50% and 100% of d
	jitter := d/2 + time.Duration(rand.Int63n(int64(d)/2))
	return jitter
}

// sleepWithContext waits for the given duration or until context is done
func sleepWithContext(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// parseRetryAfter parses the Retry-After header and returns a duration
func parseRetryAfter(resp *http.Response) time.Duration {
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 2 * time.Second
	}

	// Retry-After as seconds
	if secs, err := strconv.Atoi(ra); err == nil {
		return time.Duration(secs) * time.Second
	}

	// Retry-After as HTTP date
	if t, err := http.ParseTime(ra); err == nil {
		return time.Until(t)
	}

	// Fallback
	return 2 * time.Second
}
