package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DiskCache stores charts on disk with TTL expiration and concurrency safety.
type DiskCache struct {
	dir             string
	ttl             time.Duration
	cleanupInterval time.Duration
	mu              sync.RWMutex
	stopCh          chan struct{} // Channel to signal the cleanup routine to stop
	stopOnce        sync.Once     // Makes Stop idempotent — see Stop
	done            chan struct{} // Closed when startCleanupRoutine has returned, so a test can wait on THIS cache's goroutine
}

// Option defines the functional pattern for configuration.
type Option func(*DiskCache)

// WithDir sets the cache directory.
// Default: os.TempDir()/helm-chart-cache
func WithDir(dir string) Option {
	return func(c *DiskCache) {
		c.dir = dir
	}
}

// WithTTL sets the time-to-live for cached items.
// Default: 24 Hours
func WithTTL(d time.Duration) Option {
	return func(c *DiskCache) {
		c.ttl = d
	}
}

// WithCleanupInterval sets how often the cache scans for expired items.
// Default: 1 Hour
func WithCleanupInterval(d time.Duration) Option {
	return func(c *DiskCache) {
		c.cleanupInterval = d
	}
}

// NewDiskCache creates a new cache instance using functional options.
// It starts a background goroutine for cleanup automatically.
func NewDiskCache(opts ...Option) (*DiskCache, error) {
	// 1. Initialize with defaults
	c := &DiskCache{
		dir:             filepath.Join(os.TempDir(), "helm-chart-cache"),
		ttl:             24 * time.Hour,
		cleanupInterval: 1 * time.Hour,
		stopCh:          make(chan struct{}),
		done:            make(chan struct{}),
	}
	// 2. Apply functional options
	for _, opt := range opts {
		opt(c)
	}

	// 3. Ensure the directory exists immediately
	if err := os.MkdirAll(c.dir, 0755); err != nil {
		return nil, err
	}

	// 4. Start the background cleanup routine
	go c.startCleanupRoutine()

	return c, nil
}

// Get retrieves a chart stream from the disk cache.
// It returns an io.ReadCloser which MUST be closed by the caller to avoid file handle leaks.
// Memory Impact: Low (Streaming).
func (c *DiskCache) Get(uri, version string) (io.ReadCloser, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	path := filepath.Join(c.dir, c.key(uri, version))

	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}

	// Check TTL expiration
	if time.Since(info.ModTime()) > c.ttl {
		return nil, false
	}

	// Open the file without loading it into RAM
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}

	return f, true
}

// Set streams a chart from the source Reader to the disk cache.
// It uses atomic writes (write to temp -> rename) to ensure integrity.
func (c *DiskCache) Set(uri, version string, source io.Reader) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	filename := c.key(uri, version)
	finalPath := filepath.Join(c.dir, filename)

	// Create a temp file in the same directory to ensure atomic rename works across filesystems
	tmpFile, err := os.CreateTemp(c.dir, "tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	// Ensure cleanup of temp file if operation fails
	defer func() {
		tmpFile.Close()
		if _, err := os.Stat(tmpPath); err == nil {
			os.Remove(tmpPath)
		}
	}()

	// Stream data efficiently from source to disk (32KB chunks)
	if _, err := io.Copy(tmpFile, source); err != nil {
		return err
	}

	// Flush to disk hardware
	if err := tmpFile.Sync(); err != nil {
		return err
	}

	// Close explicitly before renaming
	tmpFile.Close()

	return os.Rename(tmpPath, finalPath)
}

// Stop halts the background cleanup goroutine to prevent leaks. It is idempotent and safe to call
// concurrently: a bare close(c.stopCh) panics with "close of closed channel" on the second call,
// which turned a harmless double-cleanup into a process crash. Stop is reached from the helm
// client's Close(), and callers legitimately end up invoking that more than once — a deferred close
// on a client variable that was reassigned, a Close in both an error path and a defer, or two
// goroutines shutting down together. Failing loudly there punished correct cleanup, so callers were
// discouraged from closing at all, which is how the cleanup goroutine leaked in the first place
// (krateo-platformops/core-provider#99). TTLCache already guards its stop channel this way; this
// makes DiskCache consistent with it.
func (c *DiskCache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stopCh)
	})
}

// Clear removes all cached files.
func (c *DiskCache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.RemoveAll(c.dir); err != nil {
		return err
	}
	return os.MkdirAll(c.dir, 0755)
}

// key generates a secure unique filename from URI and version.
func (c *DiskCache) key(uri, version string) string {
	h := sha256.Sum256([]byte(uri + ":" + version))
	return hex.EncodeToString(h[:]) + ".tgz"
}

// startCleanupRoutine runs the ticker loop.
func (c *DiskCache) startCleanupRoutine() {
	defer close(c.done)
	ticker := time.NewTicker(c.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanup()
		case <-c.stopCh:
			return
		}
	}
}

// cleanup iterates over the disk directory and removes expired items.
func (c *DiskCache) cleanup() {
	// Optimization: Read directory entries WITHOUT holding the lock.
	// This prevents blocking Get() requests during large directory scans.
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// If expired, acquire lock specifically for deletion
		if time.Since(info.ModTime()) > c.ttl {
			c.mu.Lock()
			// Double-check existence inside lock to be safe
			path := filepath.Join(c.dir, entry.Name())
			os.Remove(path)
			c.mu.Unlock()
		}
	}
}
