---
type: Log
title: plumbing history
description: Curated chronological history of the plumbing library — notable changes, decisions and incidents, newest first. Release notes stay in git tags.
resource: github.com/krateo-platformops/plumbing
tags: [history]
timestamp: 2026-08-07T00:00:00Z
---

# History (curated, newest first)

- **2026-11-12 — objectclient `Apply` and `Uninstall` report what they did** (core-provider#423):
  `Apply` returns an `ApplyResult` {`Op`, `Before`, `After`}. `Op` is `created`; `updated` (an
  Update the apiserver persisted, its resourceVersion moved); or `unchanged` (an Update the
  apiserver short-circuited, its resourceVersion unmoved, so the cluster keeps no trace of it).
  `Before` is the version the Update replaced. A dry run is never `unchanged`. `Uninstall`
  returns an `UninstallResult` {`Op`: `deleted` | `absent`, `Before`}; a Delete answered NotFound
  is `absent`. Apply still sends its unconditional Update: a caller recording changes reads the
  result rather than re-deriving them. **Breaking:** both now return a result with the error.
- **2026-11-12 — dialguard on js/wasm: requests go through fetch, and redirects are refused**
  (snowplow#1359, for the Agentiko sandbox):
  - **The bug:** on js/wasm, net/http uses the browser's fetch only for a Transport with no dial
    function. `dialguard.Transport()` always set one, so every plumbing client dialled Go's
    in-process network instead, which refuses a dialer carrying a Control hook. The JWKS never
    loaded and every token was a 503.
  - **Transport:** on js/wasm it now has no dial function, from snowplow's patch. The server
    build is unchanged, and `Dialer`/`Control` are unchanged. On js the destination policy and
    TLS client certificates don't apply, and the docs say so.
  - **Redirects:** fetch follows redirects itself, so `CheckRedirect` and the origin guards of
    `http/request` and `helm/getter` would never see a hop, and a JWKS fetch could be bounced
    anywhere. The new `dialguard.RoundTripper` sets fetch's redirect mode to `error` wherever
    net/http uses fetch, mirroring its choice (`jsFetchDisabled`: not under Node), and is the
    identity elsewhere. `Client`, `http/request` and both `helm/getter` paths (HTTP and OCI)
    use it. The OCI getter had its own transport literal with dialguard's dialer, so on js it
    could never use fetch; it's now built by `dialguard.Transport()`, which is the same on
    servers (core-provider's review).
  - **Proof:** the js tests run in CI under Node's real fetch and Go's network, against a
    redirecting server whose target must never be reached.
- **2026-11-11 — the CSR cleanup is bounded, and a failed create cleans up too** (plumbing#129,
  from the review of authn#123):
  - **Context:** the `certs` helpers take a `context.Context` first (a signature change, Diego), and
    signup passes the caller's.
  - **Bounded cleanup:** a cleanup delete runs on `context.WithoutCancel(ctx)` with
    `csrCleanupTimeout` (30s). A cancelled request still deletes the CSR it created, and an
    apiserver that never answers holds the signup 30s, not the 5 minutes of
    `certificateWaitTimeout`.
  - **Only our own CSR is deleted** (snowplow's review): the CSR's name is the user's, so
    another signup for the same user may hold it. The cleanup reads it and deletes it only if
    its `spec.request` is ours, with one DELETE carrying a UID precondition
    (`certs.DeleteCertificateSigningRequestUID`), so a CSR re-created in between is never
    touched. The cleanup delete from #123, which deleted by name, now works the same way.
  - **Failed create:** a create that fails may still have been stored (a timeout after the
    write). It is now cleaned up, if it is ours, a failed delete being joined after the
    create's error. A create refused outright, or a re-create that lost the name to
    another signup, leaves that signup's CSR alone.
- **2026-11-11 — Reconcile counts the caller's authorization annotations as content**
  (plumbing#124, AUDIT-4): audit v0.6.0's content hash counts the annotations authorization
  reads (`content_annotations`, `audit.AuthorizationAnnotations()`), but `canonicalContent`
  counted no annotation. A merge that changed only one would be a `metadataOnly` verdict
  next to a record whose `content_sha256` changed. `UpgradeConfig.ContentAnnotations` names
  them, and they're kept under `metadata.annotations` exactly as the hash keeps them: present
  with a non-null value. plumbing hard-codes no component's keys. The caller passes the
  schema's list, and an empty list keeps the previous behavior. A null member now counts as
absent at every depth, as c1 reads it, so a verdict never calls content changed whose hash
is the same. The three differences that remain on purpose (a ConfigMap's non-data fields,
RBAC as sets, a top-level empty list) are documented, and the parity test pins each one.
- **2026-11-11 — signup deletes its CSR** (authn#119 review): `signup.Do`/`DoWritten`
  created a CertificateSigningRequest under the fixed name of the identity (authn's own,
  `authn`), approved it, read the certificate and never deleted it. An approved CSR left
  under a well-known name is an attack surface, and it lingered until the apiserver's
  garbage collection. It is now deleted on every way out after its create. On success, a
  failed delete fails the signup so the caller retries; after any other failure the original
  error is kept, joined with the delete's when that fails too. A stale CSR from an earlier version is
  replaced by the existing AlreadyExists path, then deleted.
- **2026-11-11 — `usernames`: one definition of the rules for authn's usernames**: demo's
  access-request form (identity-lifecycle) has to refuse the names authn's name guard would
  refuse, at request time. The reserved names and prefixes moved here verbatim from authn's
  guard, with `Check` for a requested username: non-empty, at most 63 characters, already
  in the DNS-1123 form authn issues, and not reserved. Whether a name is claimed or bound
  is decided in-cluster only, never on a public form, so nobody can enumerate who exists.
  The prefix `krateo:` is reserved too: platform identities a component impersonates
  (snowplow's `krateo:public`) must never be issued to a person.
- **2026-11-11 — `endpoints.StoreWritten` and `signup.DoWritten` report the write**
  (authn#119): authn records its own certificate renewal as an audit record (AUDIT-4), which
  must name the Secret written, the verb sent and the apiserver's answer. `Store` sends a
  create and, when the Secret exists, an update after the 409, and returned neither, so the
  verb could only have been guessed. `StoreWritten` returns `Written{Namespace, Name, UID,
  ResourceVersion, Verb, StatusCode}` for the request that stored the Secret, and
  `signup.DoWritten` passes it through. `Store` and `Do` are unchanged.
- **2026-11-11 — Reconcile reports every changed child** (#106): to write one
  controller audit record per child (AUDIT-4), core-provider's cdc must know each
  child the self-heal merge changed, what it sent, and whether the content changed.
  `ChangedObjects` stopped at 10 and couldn't tell a content change from a
  metadata-only one. `ReconcileResult.Objects` is a separate, uncapped list of
  `ObjectVerdict`s: the resource, name and uid, the verb sent, and one of
  `created`, `deleted`, `specChanged`, `metadataOnly`, `compareFailed`, `kept`,
  `deleteNotDone` or `deleteUnknown` (an intended deletion whose live state couldn't
  be read). It says what happened and leaves what counts to the caller, and
  `ObjectsComplete` says when the list may be missing a child. Checking helm's
  update loop showed that it lists a child under `Created` before the create runs and
  under `Updated` whatever the patch's outcome. So a failed write was counted as a
  change, in the existing counts too. A write now counts only when helm refreshed
  the object from the server (it has a resourceVersion). Failed writes are
  `createFailed` and `updateFailed` verdicts, and `ObjectsNotAttempted` counts the
  children helm never reached after a failed create. Content compares as snowplow ruled: RBAC rules and subjects as
  sets, Secrets by effective data and type. The capped diagnostic lists and their
  log lines are unchanged. Kept and not-done deletions now come from one read per
  object (`pendingDeletions`), shared by the verdicts and the `Changed` decision.
- **2026-11-10 — a retired signing key is a 401, not a 503** (#63): a token signed with
  a key authn no longer publishes got `ErrKeyUnavailable`, the same as an unreachable
  JWKS, so validators answered 503 (retry) and the portal, which offers sign-in only on
  401, left users on an error page until the token expired. Revoking leaked tokens by
  retiring their key had no clean way out. `JWKSKeySource` now reports a `kid` missing
  from the set its latest attempt fetched successfully, with that set's fetch time.
  When the token was issued before that fetch (beyond the 5s leeway),
  `ValidateWithKeySource` returns `ErrTokenInvalid` and `ErrKeyNotPublished`, because
  authn publishes a key from the moment it signs with it. Everything else stays
  `ErrKeyUnavailable`: a token issued after the cached set, such as one signed with a
  new key at the rotation moment while the refresh floor blocks a refetch; a token
  without `iat`; a latest attempt that failed; and a set with no usable key. A retired
  `kid` still in a cache younger than the TTL keeps verifying until the refresh, so a
  revocation lands within the TTL (5m by default). Callers that call `PublicKey`
  directly see `ErrKeyUnavailable` as before.
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
