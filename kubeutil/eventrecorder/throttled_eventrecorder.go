package eventrecorder

import (
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	record "k8s.io/client-go/tools/events"
)

// ThrottledRecorder deduplicates events by stream identity and message state.
//
// For the same regarding/related object stream (plus event type/reason/action),
// repeated events with unchanged message are suppressed. To prevent indefinite
// silence, one unchanged event is forcibly published after maxSilence from the
// first suppression in the current suppressed streak.
type ThrottledRecorder struct {
	mu         sync.Mutex
	inner      record.EventRecorder
	maxSilence time.Duration
	now        func() time.Time
	signatures map[string]eventState
}

var _ record.EventRecorder = (*ThrottledRecorder)(nil)

const defaultMaxSilence = 5 * time.Minute

type eventState struct {
	signature         uint64
	firstSuppressedAt time.Time
}

// NewStateAwareRecorder returns a state-aware throttled recorder with the
// default max-silence interval for forced publishes.
func NewStateAwareRecorder(inner record.EventRecorder) *ThrottledRecorder {
	return NewStateAwareRecorderWithMaxSilence(inner, defaultMaxSilence)
}

// NewStateAwareRecorderWithMaxSilence returns a recorder that suppresses
// duplicate events but forces one publish when the stream stayed suppressed for
// maxSilence since the first suppression.
// If maxSilence <= 0, forced publishing is disabled and unchanged events are
// suppressed indefinitely.
func NewStateAwareRecorderWithMaxSilence(inner record.EventRecorder, maxSilence time.Duration) *ThrottledRecorder {
	return &ThrottledRecorder{
		inner:      inner,
		maxSilence: maxSilence,
		now:        time.Now,
		signatures: make(map[string]eventState),
	}
}

// NewThrottledRecorder is kept for backward compatibility.
// publishN is ignored because deduplication is state-based, not count-based.
// The default max-silence interval is used for forced publishes.
func NewThrottledRecorder(inner record.EventRecorder, _ int) *ThrottledRecorder {
	return NewStateAwareRecorder(inner)
}

func (t *ThrottledRecorder) Eventf(regarding runtime.Object, related runtime.Object, eventType, reason, action, note string, args ...interface{}) {
	if t == nil || t.inner == nil {
		return
	}

	message := fmt.Sprintf(note, args...)
	key := eventKey(regarding, related, eventType, reason, action)
	sig := hashString(message)
	now := t.now()
	shouldPublish := true

	t.mu.Lock()
	state, found := t.signatures[key]
	if found && state.signature == sig {
		shouldPublish = false
		if state.firstSuppressedAt.IsZero() {
			state.firstSuppressedAt = now
		}
		if t.maxSilence > 0 && now.Sub(state.firstSuppressedAt) >= t.maxSilence {
			shouldPublish = true
			state.firstSuppressedAt = time.Time{}
		}
	} else {
		state.signature = sig
		state.firstSuppressedAt = time.Time{}
	}
	state.signature = sig
	t.signatures[key] = state
	t.mu.Unlock()

	if !shouldPublish {
		return
	}

	t.inner.Eventf(regarding, related, eventType, reason, action, "%s", message)
}

// Reset clears all tracked event states, including suppression timers.
func (t *ThrottledRecorder) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.signatures = make(map[string]eventState)
}

// Forget removes tracked event states for a specific regarding object.
func (t *ThrottledRecorder) Forget(regarding runtime.Object) {
	prefix := objectKey(regarding) + "|"

	t.mu.Lock()
	defer t.mu.Unlock()

	for k := range t.signatures {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(t.signatures, k)
		}
	}
}

func eventKey(regarding runtime.Object, related runtime.Object, eventType, reason, action string) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s", objectKey(regarding), objectKey(related), eventType, reason, action)
}

func objectKey(obj runtime.Object) string {
	if obj == nil {
		return "<nil>"
	}

	gvk := obj.GetObjectKind().GroupVersionKind().String()
	if accessor, err := meta.Accessor(obj); err == nil {
		return fmt.Sprintf("%T|%s|%s|%s|%s", obj, gvk, accessor.GetNamespace(), accessor.GetName(), accessor.GetUID())
	}

	return fmt.Sprintf("%T|%s", obj, gvk)
}

func hashString(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
