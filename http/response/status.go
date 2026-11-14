package response

import (
	"encoding/json"
	"net/http"
)

// Values of Status.Status
const (
	StatusSuccess = "Success"
	StatusFailure = "Failure"
)

// StatusReason is an enumeration of possible failure causes.  Each StatusReason
// must map to a single HTTP status code, but multiple reasons may map
// to the same HTTP status code.
type StatusReason string

const (
	// StatusReasonUnknown means the server has declined to indicate a specific reason.
	// Status code 500.
	StatusReasonUnknown StatusReason = ""

	// StatusReasonUnauthorized means the server can be reached and understood the request, but requires
	// the user to present appropriate authorization credentials.
	// Status code 401
	StatusReasonUnauthorized StatusReason = "Unauthorized"

	// StatusReasonForbidden means the server can be reached and understood the request, but refuses
	// to take any further action.
	// Status code 403
	StatusReasonForbidden StatusReason = "Forbidden"

	// StatusReasonNotFound means one or more resources required for this operation
	// could not be found.
	// Status code 404
	StatusReasonNotFound StatusReason = "NotFound"

	// StatusReasonConflict means the requested operation cannot be completed
	// due to a conflict in the operation.
	// Status code 409
	StatusReasonConflict StatusReason = "Conflict"

	// StatusReasonGone means the item is no longer available at the server and no
	// forwarding address is known.
	// Status code 410
	StatusReasonGone StatusReason = "Gone"

	// StatusReasonInvalid means the requested create or update operation cannot be
	// completed due to invalid data provided as part of the request.
	// Status code 422
	StatusReasonInvalid StatusReason = "Invalid"

	// StatusReasonTimeout means that the request could not be completed within the given time.
	// Status code 504
	StatusReasonTimeout StatusReason = "Timeout"

	// StatusReasonTooManyRequests means the server experienced too many requests within a
	// given window and that the client must wait to perform the action again.
	// Status code 429
	StatusReasonTooManyRequests StatusReason = "TooManyRequests"

	// StatusReasonBadRequest means that the request itself was invalid, because the request
	// doesn't make any sense.
	// Status code 400
	StatusReasonBadRequest StatusReason = "BadRequest"

	// StatusReasonMethodNotAllowed means that the action the client attempted to perform not supported.
	// Status code 405
	StatusReasonMethodNotAllowed StatusReason = "MethodNotAllowed"

	// StatusReasonNotAcceptable means that the accept types indicated by the client were not acceptable
	// to the server.
	// Status code 406
	StatusReasonNotAcceptable StatusReason = "NotAcceptable"

	// StatusReasonRequestEntityTooLarge means that the request entity is too large.
	// Status code 413
	StatusReasonRequestEntityTooLarge StatusReason = "RequestEntityTooLarge"

	// StatusReasonUnsupportedMediaType means that the content type sent by the client is not acceptable.
	// Status code 415
	StatusReasonUnsupportedMediaType StatusReason = "UnsupportedMediaType"

	StatusUnprocessableEntity StatusReason = "UnprocessableEntity"

	// StatusReasonInternalError indicates that an internal error occurred.
	// Status code 500
	StatusReasonInternalError StatusReason = "InternalError"

	// StatusReasonServiceUnavailable means that the request itself was valid,
	// but the requested service is unavailable at this time.
	// Status code 503
	StatusReasonServiceUnavailable StatusReason = "ServiceUnavailable"
)

// Status is a return value for calls that don't return other objects.
type Status struct {
	Kind       string `json:"kind,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`

	// Status of the operation.
	// One of: "Success" or "Failure".
	Status string `json:"status,omitempty"`
	// A human-readable description of the status of this operation.
	Message string `json:"message,omitempty"`
	// A machine-readable description of why this operation is in the
	// "Failure" status. If this value is empty there
	// is no information available. A Reason clarifies an HTTP status
	// code but does not override it.
	Reason StatusReason `json:"reason,omitempty"`
	// Extended data associated with the reason. Each reason may define its
	// own extended details. It is optional: nil unless a caller sets it, and
	// a nil Details is omitted from the body, so a Status without Details
	// encodes exactly as it did before the field existed. New never sets it.
	// It sits between reason and code, where metav1.Status has it.
	Details *StatusDetails `json:"details,omitempty"`
	// Suggested HTTP return code for this status, 0 if not set.
	Code int `json:"code,omitempty"`
}

// StatusDetails is a set of additional properties that MAY be set by the
// server to provide additional information about a response. The Reason field
// of a Status object defines what attributes will be set.
//
// It mirrors k8s.io/apimachinery's metav1.StatusDetails field for field and
// JSON tag for JSON tag (details_test.go asserts it by reflection), so a body
// carrying it decodes as a metav1.Status in a client-go reader, and a
// metav1.Status body decodes here without loss.
type StatusDetails struct {
	// The name attribute of the resource associated with the status
	// StatusReason (when there is a single name which can be described).
	Name string `json:"name,omitempty"`
	// The group attribute of the resource associated with the status
	// StatusReason.
	Group string `json:"group,omitempty"`
	// The kind attribute of the resource associated with the status
	// StatusReason. On some operations may differ from the requested resource
	// Kind.
	Kind string `json:"kind,omitempty"`
	// UID of the resource (when there is a single resource which can be
	// described).
	UID string `json:"uid,omitempty"`
	// The Causes array includes more details associated with the StatusReason
	// failure. Not all StatusReasons may provide detailed causes.
	Causes []StatusCause `json:"causes,omitempty"`
	// If specified, the time in seconds before the operation should be
	// retried.
	RetryAfterSeconds int32 `json:"retryAfterSeconds,omitempty"`
}

// CauseType is a machine readable value providing more detail about what
// occurred in a status response. An operation may have multiple causes for a
// status (whether Failure or Success).
type CauseType string

// StatusCause provides more information about a Status failure, including
// cases when multiple errors are encountered. It mirrors metav1.StatusCause.
type StatusCause struct {
	// A machine-readable description of the cause of the error. If this value
	// is empty there is no information available. Its JSON name is "reason",
	// as in metav1.StatusCause.
	Type CauseType `json:"reason,omitempty"`
	// A human-readable description of the cause of the error. This field may
	// be presented as-is to a reader.
	Message string `json:"message,omitempty"`
	// The field of the resource that has caused this error, as named by its
	// JSON serialization. May include dot and postfix notation for nested
	// attributes.
	Field string `json:"field,omitempty"`
}

func New(code int, err error) *Status {
	res := &Status{
		Kind:       "Status",
		APIVersion: "v1",
		Code:       code,
	}

	if err != nil {
		res.Message = err.Error()
	}

	switch code {
	case http.StatusUnprocessableEntity:
		res.Status = StatusFailure
		res.Reason = StatusUnprocessableEntity
	case http.StatusUnauthorized:
		res.Status = StatusFailure
		res.Reason = StatusReasonUnauthorized
	case http.StatusForbidden:
		res.Status = StatusFailure
		res.Reason = StatusReasonForbidden
	case http.StatusNotFound:
		res.Status = StatusFailure
		res.Reason = StatusReasonNotFound
	case http.StatusConflict:
		res.Status = StatusFailure
		res.Reason = StatusReasonConflict
	case http.StatusGone:
		res.Status = StatusFailure
		res.Reason = StatusReasonGone
	case http.StatusNotImplemented:
		res.Status = StatusFailure
		res.Reason = StatusReasonInvalid
	case http.StatusBadRequest:
		res.Status = StatusFailure
		res.Reason = StatusReasonBadRequest
	case http.StatusServiceUnavailable:
		res.Status = StatusFailure
		res.Reason = StatusReasonServiceUnavailable
	case http.StatusNotAcceptable:
		res.Status = StatusFailure
		res.Reason = StatusReasonNotAcceptable
	case http.StatusMethodNotAllowed:
		res.Status = StatusFailure
		res.Reason = StatusReasonMethodNotAllowed
	case http.StatusInternalServerError:
		res.Status = StatusFailure
		res.Reason = StatusReasonInternalError
	case http.StatusRequestEntityTooLarge:
		res.Status = StatusFailure
		res.Reason = StatusReasonRequestEntityTooLarge
	case http.StatusUnsupportedMediaType:
		res.Status = StatusFailure
		res.Reason = StatusReasonUnsupportedMediaType
	default:
		res.Status = StatusSuccess
	}

	return res
}

func AsMap(s *Status) (map[string]any, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return map[string]any{}, err
	}

	var dict map[string]any
	err = json.Unmarshal(data, &dict)
	if err != nil {
		return map[string]any{}, err
	}

	return dict, nil
}
