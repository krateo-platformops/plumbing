---
type: API
title: plumbing exported API
description: The exported package map — one line per package, grouped by theme, derived from the package docs and code; full godoc lives on pkg.go.dev.
resource: https://pkg.go.dev/github.com/krateo-platformops/plumbing
tags: [go-api, packages]
timestamp: 2026-08-07T00:00:00Z
---

# Exported API

The contract is the exported surface of every Go package below. This page is the
**map**, not the reference — full signatures and doc comments live at
[pkg.go.dev/github.com/krateo-platformops/plumbing](https://pkg.go.dev/github.com/krateo-platformops/plumbing)
(or `go doc github.com/krateo-platformops/plumbing/<pkg>`). One line per package,
derived from the actual package docs and code.

## Helm engine

| Package | What it exports |
|---|---|
| `helm` | The provider-agnostic `Client` interface (Install/Upgrade/**Reconcile**/Uninstall/Rollback/GetRelease/ListReleases) plus config, `Release`/`ReconcileResult` and `PostRenderer` types. |
| `helm/v3` | The Helm-v3-SDK implementation: cached action configs, CRD-informer discovery invalidation (`NewCRDInformer`), duplicate-resource post-render validation, and the fork-free apply-if-changed `Reconcile` (see [overview](./overview.md)). |
| `helm/v3/tracer` | An `http.RoundTripper` that logs each Kubernetes request a Helm operation makes and collects the touched `Resource`s. |
| `helm/getter` | Chart fetching (`Get`, `NewOCIGetter`) with credentials, TLS, timeout, repo and disk-cache options. |
| `helm/getter/cache` | `DiskCache` — content-addressed on-disk cache for fetched charts. |
| `helm/getter/repo` | Classic HTTP chart-repository index types (`IndexFile`, `ChartVersions`) and URL helpers. |
| `helm/utils` | Post-render helpers: composition `LabelsPostRender`, the `krateo.io/traceparent` stamp (scoped to `*.krateo.io`), the `krateo.io/gracefully-paused` annotation key. |

## Kubernetes utilities

| Package | What it exports |
|---|---|
| `kubeutil` | Misc helpers: DNS-1123 name mangling, `DetectClusterName`, `ServiceAccountNamespace`, `ConfigMapData`, in-cluster CA cert. |
| `kubeutil/objectclient` | Retrying, `dynamic.Interface`-based create-or-update (`Apply` — writes the server response back into the object), `Get`, delete-if-present. |
| `kubeutil/hasher` | Cumulative, order-dependent hash over JSON-marshalable values — desired-vs-deployed drift detection. |
| `kubeutil/rbacgen` | Builds least-privilege Role/RoleBinding sets granting one ServiceAccount access to an exact named resource set. |
| `kubeutil/rbac` | Batched `UserCan` permission checks (SelfSubjectAccessReview) with a TTL cache. |
| `kubeutil/dynamicwatch` | Registers a controller-runtime watch on a GVK not known until runtime, deduping repeat registrations. |
| `kubeutil/secretref` | Reads one key out of a Secret via `dynamic.Interface` (for schema-unknown-at-compile-time reconcilers). |
| `kubeutil/plurals` | GVK → plural/singular resource-name resolution (`Get`, `ResolveAPINames`). |
| `kubeutil/event` | `APIRecorder` + `Normal`/`Warning` event constructors for controller runtimes. |
| `kubeutil/eventrecorder` | `record.EventRecorder` factories, including a state-aware throttled recorder. |
| `kubeutil/discoveryevents` | Publishes API-discovery resource added/changed/removed events onto an `eventbus.Bus`. |
| `labels` | THE shared composition label keys coupling core-provider and composition-dynamic-controller (compile-time cross-repo contract). |

## CRD generation

| Package | What it exports |
|---|---|
| `crdgen` | `Generate(Options)` — direct JSON-Schema → structural-OpenAPI-v3 CRD transpiler, gated on the apiextensions validation library; `NormalizeVersionName`. |
| `crdgen/schemas` | JSON-Schema resolution primitives and type-name constants used by the transpiler. |

## Identity, endpoints, auth

| Package | What it exports |
|---|---|
| `jwtutil` | RS256 Krateo JWT create/verify, against a static key or authn's JWKS, with `UserInfo` claims. A delegated token also carries RFC 8693's `act` (`Actor`), read through `UserInfo.Actor()` and `DelegationChain()`. `ValidateClaimsWithKeySource` returns all the verified claims, `exp` included. |
| `endpoints` | The Krateo `Endpoint` (user API-server credential record) stored/loaded as a Secret. |
| `kubeconfig` | `Endpoint` → kubeconfig YAML (`Marshal`) and → `*rest.Config` (`NewClientConfig`). |
| `signup` | Creates a certificate-based cluster user (CSR flow) and stores its `Endpoint`. |
| `certs` | CertificateSigningRequest helpers: create, approve, wait, fetch the issued cert. |

## HTTP client & server

| Package | What it exports |
|---|---|
| `http/request` | Outbound request building incl. AWS Signature v4 header computation. |
| `http/response` | The `Status` response envelope (`Success`/`Failure`) + typed error writers (`BadRequest`, `Forbidden`, …). |
| `http/util` | `RetryClient` — retrying, rate-limited `http.Client` wrapper. |
| `server/probes` | `/livez` + `/readyz` handlers and a `HealthServer`. |
| `server/use` | Middleware: access logging, CORS, trace-id propagation. |
| `server/use/cors` | The CORS handler implementation used by `server/use`. |

## General utilities

| Package | What it exports |
|---|---|
| `bufferpool` | Sized `sync.Pool`-backed byte-buffer pool. |
| `cache` | Generic `TTLCache[K,V]` with cleanup interval and max-entries options. |
| `codegen` | Fluent Go source-code generation builders (`Package`, `Function`, `If`, …). |
| `context` | Krateo request context: trace-id (`X-Krateo-TraceId`), per-request logger, access token, user config/info. |
| `deps` | Small dependency `Graph` with topological resolution. |
| `env` | Typed env readers (`String`, `Bool`, `Int`, `Duration`, `ServicePort`, …) with defaults. |
| `eventbus` | In-process publish/subscribe `Bus`. |
| `jqutil` | gojq evaluation helpers: `Eval`, `Extract`, `ForEach`, module loading, type inference. |
| `logger` | JSON `slog.Logger` factory with a service-name attribute. |
| `maps` | Nested-map access/copy (`NestedString`, `DeepCopyJSON`, `LeafPaths`, …). |
| `pgutil` | PostgreSQL connection-URL building and wait-for-ready pool creation (pgx). |
| `ptr` | Generic pointer helpers (`To`, `Deref`, `Equal`). |
| `shortid` | Short, unique, non-sequential URL-friendly id generation. |
| `wait` | Generic backoff retry `Until` / `UntilWithOptions`. |
| `e2e` | e2e-framework helpers for consumer test suites: namespaces, coverage, sign-up step. |

Compatibility note: within the v1 line, exported APIs may still move between minors
(e.g. the slogs→slog removal, the crdgen transpiler replacement) — this is an
internal platform library, and consumers pin exact tags precisely for this reason
(see [usage](./usage.md)).
