---
type: Log
title: plumbing history
description: Curated chronological history of the plumbing library — notable changes, decisions and incidents, newest first. Release notes stay in git tags.
resource: github.com/krateo-platformops/plumbing
tags: [history]
timestamp: 2026-08-07T00:00:00Z
---

# History (curated, newest first)

- **2026-11-08 — jwtutil `principal`**: `UserInfo`, `Actor` and `CreateTokenOptions`
  gain an optional `principal` claim, one of `human`, `agent`, `controller` or
  `service`. authn sets it from how a token was obtained, so the shared audit schema
  (installer#716) can record what kind of principal acted. On a delegated token the
  subject stays `human`, and each `act` link carries its actor's kind;
  `ActorPrincipal()` returns the outermost one. `CreateToken` refuses any other value.
  Reading is lenient: an absent, unknown or non-string kind reads as `""` and never
  fails validation, so older tokens and newer issuers don't break validators. Tokens
  created without it are unchanged.
- **2026-11-08 — `endpoints.SecretName`**: the session Secret's name,
  `<DNS-1123 username>-clientconfig`, was rebuilt by hand in `endpoints.Store`,
  `server/use/userconfig.go` and authn. Core-provider now needs it too, to delete a
  cdc's stale credential, where a near-miss deletes nothing and says nothing.
  `endpoints.SecretName(username)` is its one definition, and both plumbing call sites
  use it.
- **2026-11-08 — jwtutil `ValidateClaimsWithKeySource`**: verifies exactly like
  `ValidateWithKeySource`, which now wraps it, and returns the full `KrateoClaims`,
  registered claims such as `ExpiresAt` included. authn's `/delegate` needs the subject
  token's verified expiry, so a delegated token never outlives it; reading it from an
  unverified parse was CodeQL's `go/missing-jwt-signature-check`.
- **2026-11-08 — jwtutil `act`**: `UserInfo` and `CreateTokenOptions` gain an optional
  RFC 8693 `act` claim (`Actor`, nested for a chain). It's for authn's delegated tokens
  (AUDIT-6, installer#412): the subject stays the human, and `act` names the agent
  acting for them. Validators read `Actor()` and `DelegationChain()`. Tokens without
  `act` are unchanged, and a chain with an unnamed actor, or deeper than
  `MaxActorDepth` (8), is refused on create and on validate.
- **2026-08-07 — docs**: adopted the Krateo Documentation Standard (this bundle);
  `lint-docs` wired into CI.
- **2026-08-05 — envtest CI**: functional cache-staleness coverage for `helm/v3`
  runs against a real kube-apiserver (`make test-envtest`, `-tags envtest`) in CI
  alongside the race-enabled unit suite (#18).
- **2026-08-04 — v1.13.2**: `helm/v3` `Install` retries once on a stale-discovery
  REST-mapping miss (chart installed immediately after its CRD lands).
- **2026-08-04 — v1.13.1**: `kubeutil/objectclient.Apply` writes the server
  response back into the object — the fix for core-provider's
  CompositionDefinition deployed-digest gate never converging (desired hash was
  computed over the bare render instead of the server-defaulted object).
- **2026-08-03 — v1.13.0**: module identity migrated to
  `github.com/krateo-platformops/plumbing` (org independence; no `/v2` suffix).
- **2026-07-27 — v1.12.0**: five new `kubeutil` packages — `hasher`,
  `objectclient`, `secretref`, `rbacgen`, `dynamicwatch` — extracted as shared
  controller tooling.
- **2026-07-24 — v1.10.7 / v1.11.0**: crdgen sanitizes type-less object nodes so
  rich third-party schemas yield valid CRDs (#11); `NormalizeVersionName` exported
  (#12); the **direct JSON-Schema → structural-OpenAPI-v3 transpiler** replaced
  the legacy crdgen path (#15 RFC, #16) — see
  [crdgen/docs/ref-resolution-redesign.md](../crdgen/docs/ref-resolution-redesign.md).
- **2026-07-12 — v1.10.6**: `Upgrade` propagates `TakeOwnership` (self-healing
  adoption was silently dropped).
- **2026-07-11 — v1.10.5**: `labels` package created — the shared home for the
  cross-repo composition label keys (compile-time contract between core-provider
  and composition-dynamic-controller).
- **2026-07-09..10 — v1.10.0..v1.10.3**: the **apply-if-changed `Reconcile`**
  landed with semantic change-detection, then hardened: don't wedge on a child's
  CRD-version migration, adopt live-owned children missing from the stored
  manifest, repair the stored manifest against unserved GVKs. This is the engine
  behind CDC's no-churn self-healing Observe.
- **2026-07 — traceparent scoping**: the `krateo.io/traceparent` post-render stamp
  was scoped to `*.krateo.io` resources only — stamping every child made GKE
  re-ensure LoadBalancer Services on every reconcile (IP reserve/release thrash).
  Shipped on the `v1.7.x` maintenance line (extended through `v1.7.16`) while
  `main` was ahead.
- **2026-04-22 — v1.8.0**: the pretty logger was removed in favor of stdlib
  `slog` JSON output (a brief v1.10.x restore of `slogs/pretty` was reverted).
- **2026-02..06 — v1.0.0..v1.9.0**: the Helm `Client` introduced (v1.0.0) and
  grown: discovery cache with CRD informer (v1.6.0), REST-config + chart caching
  (v1.7.1), duplicate-resource validation (v1.7.2); crdgen fixes (concurrent
  generation race, int-or-string, min/max lengths); AWS Signature v4 request
  support; eventbus + discovery events; throttled event recorder; probes, wait,
  pgutil.
- **pre-v1.0.0**: utility-belt era (`v0.x`) — jqutil, maps, endpoints, jwtutil,
  shortid and friends accumulated as the platform services were extracted.
