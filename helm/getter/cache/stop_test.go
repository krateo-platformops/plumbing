package cache

import (
	"sync"
	"testing"
	"time"
)

// TestStop_Idempotent pins the guard added for krateo-platformops/core-provider#99: Stop used to
// call close(c.stopCh) directly, so a second call panicked with "close of closed channel". Stop is
// reached from the helm client's Close(), and callers legitimately invoke that more than once (a
// deferred close on a reassigned client variable, a Close in both an error path and a defer), so
// correct cleanup could crash the process.
func TestStop_Idempotent(t *testing.T) {
	c, err := NewDiskCache(WithDir(t.TempDir()), WithCleanupInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop must be idempotent, but a repeated call panicked: %v", r)
		}
	}()

	for i := 0; i < 5; i++ {
		c.Stop()
	}
}

// TestStop_ConcurrentIsSafe covers the other way the bare close blew up: two shutdown paths racing.
// Run under -race to also catch unsynchronised access.
func TestStop_ConcurrentIsSafe(t *testing.T) {
	c, err := NewDiskCache(WithDir(t.TempDir()), WithCleanupInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	panics := make(chan any, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			c.Stop()
		}()
	}
	wg.Wait()
	close(panics)

	if r, ok := <-panics; ok {
		t.Fatalf("concurrent Stop panicked: %v", r)
	}
}

// TestStop_TerminatesCleanupGoroutine is the point of calling Stop at all: NewDiskCache starts
// startCleanupRoutine unconditionally, and never stopping it is what leaked a goroutine per helm
// client in core-provider#99. This asserts the goroutine is actually gone after Stop, so the guard
// above cannot be "fixed" by making Stop a no-op.
//
// It waits on THIS cache's done channel. It used to count every goroutine in startCleanupRoutine
// across the process, in a 1 MiB stack dump: other tests' caches moved the count (before=361,
// now=1003 in one run), and past ~1 MiB of stacks the dump truncated and the count itself was wrong.
func TestStop_TerminatesCleanupGoroutine(t *testing.T) {
	c, err := NewDiskCache(WithDir(t.TempDir()), WithCleanupInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	// Control: the goroutine is running before Stop, across several ticks. Without this, a done
	// channel closed at construction would pass the assertion below.
	select {
	case <-c.done:
		t.Fatal("cleanup goroutine exited before Stop was called")
	case <-time.After(50 * time.Millisecond):
	}

	c.Stop()

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Error("cleanup goroutine still running 2s after Stop: the DiskCache goroutine leak from core-provider#99 is back")
	}
}
