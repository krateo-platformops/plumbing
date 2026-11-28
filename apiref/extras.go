// Package apiref holds the rules for the request an apiRef RESTAction is resolved with, so that every
// binary that must agree on it computes it from one function.
package apiref

// Keys a composition's per-instance context occupies in an apiRef's extras.
const (
	ExtraCompositionName      = "compositionName"
	ExtraCompositionNamespace = "compositionNamespace"
	ExtraCompositionID        = "compositionId"
)

// CompositionExtras returns the extras sent to snowplow for one composition: a COPY of static, with
// the composition's name, namespace and UID set over it (per-instance context wins over static
// extras). static is never mutated and may be nil.
//
// static is the CompositionDefinition's apiRef.extras, which core-provider hands to the composition
// controller as the environment variable COMPOSITION_CONTROLLER_API_REF_EXTRAS (read by
// MemberFromConfig in the controller's internal/manager/memberconfig.go).
//
// The composition-dynamic-controller sends the result on snowplow's /call, and core-provider sends
// the same result to snowplow's GET /rbac to learn which namespaces the composition's reads land in;
// the two must be byte-for-byte equal, so both call this function.
func CompositionExtras(static map[string]any, name, namespace, uid string) map[string]any {
	out := make(map[string]any, len(static)+3)
	for k, v := range static {
		out[k] = v
	}
	out[ExtraCompositionName] = name
	out[ExtraCompositionNamespace] = namespace
	out[ExtraCompositionID] = uid
	return out
}
