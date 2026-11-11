---
type: Architecture
title: plumbing architecture
description: Package design of the shared Go library — the helm apply-if-changed Reconcile engine, crdgen, the kubeutil family, the shared label contract — and how core-provider and composition-dynamic-controller consume them.
resource: github.com/krateo-platformops/plumbing
tags: [helm, reconcile, crdgen, labels]
timestamp: 2026-08-07T00:00:00Z
---

# Architecture

plumbing is a flat collection of small, independent Go packages with one module
identity (`github.com/krateo-platformops/plumbing`, see [go.mod](../go.mod)). There is
no binary and no shared framework: each package is importable on its own, and the
library's real job is to be the **single home for code and contracts that would
otherwise drift across the platform repos**. Three packages carry that weight
explicitly; the rest are conventional utilities (mapped one-per-line in
[api](./api.md)).

## 1. The helm engine (`helm`, `helm/v3`, `helm/getter`, `helm/utils`)

`helm` defines the provider-agnostic `Client` interface (Install / Upgrade /
**Reconcile** / Uninstall / Rollback / GetRelease / ListReleases) plus its config and
result types. `helm/v3` implements it on the unforked Helm v3 SDK, adding what a
controller needs and upstream Helm does not provide:

- **cached, invalidation-aware clients** — action configs and discovery are cached
  per target; a CRD informer (`NewCRDInformer` / `WithCRDInformer`) invalidates the
  discovery/REST-mapper cache when CRDs change, and `Install` retries once on a
  stale-discovery REST-mapping miss (v1.13.2) so a chart installed right after its
  CRD lands does not fail spuriously;
- **post-render validation** — duplicate-resource detection in rendered manifests;
- **`helm/utils` post-renderers** — the composition labels post-renderer, and the
  `krateo.io/traceparent` stamp deliberately scoped to `*.krateo.io` children only
  (stamping everything made GKE re-ensure LoadBalancer Services every minute);
- **`helm/getter`** — OCI/repo chart fetching with credentials, TLS options and a
  content-addressed disk cache (`helm/getter/cache`).

### The apply-if-changed `Reconcile` (why CDC does not fork helm)

`Client.Reconcile` ([helm/v3/reconcile.go](../helm/v3/reconcile.go)) is a fork-free,
self-healing reconcile built ONLY on Helm's exported API. Per cycle:

1. Fetch the stored release (its `.Manifest` = last-applied state). No release yet →
   delegate to `Upgrade(Install:true)`, report `Changed`.
2. Repair the stored manifest against GVKs the cluster no longer serves (a
   CompositionDefinition version bump prunes the old served CRD version; without the
   repair, helm's own current-manifest build deadlocks hard).
3. Render the **target** manifest via a server-side dry-run Upgrade (real render
   pipeline — post-renderer, lookups, server validation — no revision, no hooks).
4. Snapshot each target object's live state; neutralize volatile non-semantic diffs
   (copy the live `traceparent` onto the target, fold Secret `stringData` into
   `data` as the apiserver does); adopt live-owned children missing from the stored
   manifest.
5. `KubeClient.UpdateThreeWayMerge(current, target)` — Helm's own 3-way merge
   **converges the cluster**: recreates children deleted out-of-band, patches
   drifted fields.
6. Decide `changed` by **semantic** before/after comparison with write-volatile and
   server-owned fields stripped — not `resourceVersion` deltas, which over-count and
   churned a Helm revision every cycle at steady state.
7. Only if changed: run ONE real `Upgrade` to write the revision and run hooks with
   correct ordering. Otherwise return `Changed:false` — no revision, no hooks.

What the merge did to each child is in `ReconcileResult.Objects` (plumbing#106): one
uncapped `ObjectVerdict` per child it created, deleted, changed, kept
(`helm.sh/resource-policy: keep`) or failed to delete. Each verdict carries the
group, version, resource, kind, namespace, name, uid when known, the verb sent
(`create`, `patch`, `update` under `Force`, `delete`) and the `Change`:

| Change | Meaning |
|---|---|
| `created` | absent, now created |
| `deleted` | pruned: left the render and deleted |
| `specChanged` | content changed: the fields other than apiVersion, kind, metadata, status |
| `metadataOnly` | only labels, annotations or other metadata changed |
| `compareFailed` | applied, but before/after couldn't be compared (Reconcile counts it as a change, fail safe) |
| `kept` | left the render; helm declined the delete (keep) |
| `deleteNotDone` | left the render, still live, not kept: the delete failed or never ran |
| `deleteUnknown` | left the render, not reported deleted, and its live state couldn't be read |

The verdict states what happened. Whether it counts (for an audit, a revision, a metric)
is the caller's policy.

Content compares canonically: RBAC rules, aggregation selectors and binding subjects
are sets, and a Secret compares by its effective `data` (after `stringData` folds in)
and `type`. Created, deleted and patched in the counts equal the number of
`created`, `deleted` and other surviving-child verdicts. A child the merge left
unchanged is absent, and so is a Job kept out of the merge. The list holds names
only, never values, and it is uncapped for one audit record per entry (AUDIT-4).
So log the capped `ChangedObjects`, never `Objects`. `ObjectsComplete` is false on the
install fallback in step 1, which reports no per-object outcome (derive creates from the
installed release's manifest), and when an intended deletion can't be resolved to its
object.

The steady-state guarantee is "no cluster mutation → no Helm revision, no hooks",
which is what stopped the platform's per-60s revision churn. It is only as no-churn
as the charts are idempotent (e.g. `lookup`-guarded random passwords).

## 2. `crdgen` — JSON Schema → CRD

`crdgen.Generate` transpiles a chart's `values.schema.json` directly into a
structural OpenAPI-v3 CRD ([crdgen/transpile.go](../crdgen/transpile.go)): `$ref`s
inlined by JSON pointer, cycles broken with `x-kubernetes-preserve-unknown-fields`,
tractable validation keywords carried over (some as generated CEL), and the output
gated in-process on the `k8s.io/apiextensions-apiserver` validation library — so a
generated CRD is structurally valid by construction. Design record:
[crdgen/docs/ref-resolution-redesign.md](../crdgen/docs/ref-resolution-redesign.md).
`NormalizeVersionName` maps a chart semver to the CRD version name (`1.0.0` →
`v1-0-0`). See it run in [examples/minimal](../examples/minimal/README.md).

## 3. `labels` — the cross-repo contract

[labels](../labels/labels.go) declares, once, the composition label keys that couple
core-provider and composition-dynamic-controller: core-provider stamps each
composition instance with its owning CompositionDefinition coordinates and served
version; the per-version CDC selects instances by the same keys. One byte of drift
would make version migration silently select nothing — importing the keys from one
package turns the agreement into a compile-time guarantee.

## 4. The kubeutil family and the rest

`kubeutil/*` holds controller-grade helpers: `objectclient` (retrying dynamic
create-or-update whose `Apply` writes the server response back into the object —
the v1.13.1 fix that unwedged core-provider's deployed-digest gate), `hasher`
(cumulative drift hash), `rbacgen` (least-privilege Role/RoleBinding generation),
`dynamicwatch` (watch a GVK unknown until runtime), `secretref`, `plurals`,
`eventrecorder` (throttled), `rbac` (batched SelfSubjectAccessReview-style checks
with cache). Alongside: identity plumbing (`jwtutil`, `kubeconfig`, `endpoints`,
`signup`, `certs`) used by authn/snowplow, HTTP server/client helpers
(`server/*`, `http/*`), and generic utilities (`cache`, `maps`, `jqutil`, `env`,
`ptr`, `wait`, `shortid`, `logger`, `eventbus`, `pgutil`, `e2e`).

## How the platform consumes it

- **core-provider** (engine, v1.13.2 pin): `crdgen` for CRD generation per
  CompositionDefinition; `kubeutil/objectclient` + `hasher` + `rbacgen` in its
  deploy tooling; `plurals`, `eventrecorder`, `labels`.
- **composition-dynamic-controller**: `helm/v3` end to end — `NewClient` per
  target, `Client.Reconcile` as its Observe-phase apply-if-changed self-heal,
  `helm/utils` post-renderers, `helm/getter` for chart fetch; plus `labels`,
  `dynamicwatch`, `secretref`.
- **chart-inspector, snowplow, authn, and the other platform services** use the
  identity, HTTP and utility packages.

Consumers pin explicit tags and bump deliberately — see [usage](./usage.md).
