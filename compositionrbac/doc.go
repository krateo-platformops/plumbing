// Package compositionrbac derives the RBAC a composition's helm install needs, from what
// chartinspector reports the composition's chart renders, and applies it: Roles and
// RoleBindings per namespace, a ClusterRole and ClusterRoleBinding for cluster-scoped resources,
// each bound to a named ServiceAccount, plus the namespaces the render targets.
//
// It is not a replacement for kubeutil/rbacgen, which builds single RBAC objects from arguments
// a caller already has. This package decides WHICH rules a composition needs, from a render.
//
// Moved from the composition-dynamic-controller's internal/rbacgen and internal/tools/rbac, so
// that every component deriving composition RBAC runs the same derivation.
package compositionrbac
