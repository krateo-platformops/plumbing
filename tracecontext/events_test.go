package tracecontext

import "testing"

// The strings are a cross-runtime contract read by queries and sampler rules, so the test pins the
// bytes rather than re-deriving them. A rename must fail here and be made with every reader.
func TestSharedEventAndAttributeStrings(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{EventReconcilePaused, "reconcile.paused"},
		{EventMarkerFailed, "trace.marker_failed"},
		{EventConflict, "k8s.conflict"},
		{string(AttrStep), "provider_runtime.step"},
		{string(AttrActed), "provider_runtime.acted"},
	} {
		if c.got != c.want {
			t.Errorf("contract string changed: %q, want %q", c.got, c.want)
		}
	}
}
