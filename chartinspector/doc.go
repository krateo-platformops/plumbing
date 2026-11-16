// Package chartinspector is the client for chart-inspector, the service that renders a
// composition's helm chart for one composition and reports the Kubernetes resources the render
// contains (group, version, resource, namespace, and the verbs helm needs on each).
//
// It is the input to compositionrbac: chart-inspector answers "what would this composition's
// chart create", and compositionrbac turns that into the RBAC a controller needs to install it.
// Moved from the composition-dynamic-controller's internal/chartinspector so the same client
// serves every caller that derives that RBAC.
package chartinspector
