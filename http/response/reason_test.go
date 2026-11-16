package response

import (
	"net/http"
	"testing"
)

// New gives each failure the reason apimachinery's NewGenericServerResponse gives the same code
// (plumbing#91): 504 is Timeout, 429 is TooManyRequests, and any other 5xx, 502 among them, is
// InternalError. Status mirrors metav1.Status, which has no BadGateway reason; Code still says 502.
// The codes New named before keep their reasons, and a 4xx with no Kubernetes reason keeps none.
func TestNew_ReasonFollowsApimachinery(t *testing.T) {
	for _, tc := range []struct {
		code   int
		status string
		reason StatusReason
	}{
		{http.StatusGatewayTimeout, StatusFailure, StatusReasonTimeout},
		{http.StatusTooManyRequests, StatusFailure, StatusReasonTooManyRequests},
		{http.StatusBadGateway, StatusFailure, StatusReasonInternalError},
		{http.StatusHTTPVersionNotSupported, StatusFailure, StatusReasonInternalError},
		{599, StatusFailure, StatusReasonInternalError},
		// unchanged: named before, or a 4xx Kubernetes gives no reason, or a success
		{http.StatusInternalServerError, StatusFailure, StatusReasonInternalError},
		{http.StatusServiceUnavailable, StatusFailure, StatusReasonServiceUnavailable},
		{http.StatusNotImplemented, StatusFailure, StatusReasonInvalid},
		{http.StatusTeapot, StatusFailure, StatusReasonUnknown},
		{http.StatusFailedDependency, StatusFailure, StatusReasonUnknown},
		{http.StatusFound, StatusFailure, StatusReasonUnknown},
		{0, StatusFailure, StatusReasonUnknown},
		{http.StatusOK, StatusSuccess, StatusReasonUnknown},
	} {
		got := New(tc.code, nil)
		if got.Status != tc.status || got.Reason != tc.reason || got.Code != tc.code {
			t.Errorf("New(%d): status=%q reason=%q code=%d, want status=%q reason=%q",
				tc.code, got.Status, got.Reason, got.Code, tc.status, tc.reason)
		}
	}
}
