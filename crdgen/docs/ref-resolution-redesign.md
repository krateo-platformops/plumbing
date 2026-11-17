# crdgen redesign: faithful `$ref` resolution via a direct JSON-Schema → structural-OpenAPI-v3 transpiler

**Status:** proposed (design)
**Scope:** `crdgen` package (`github.com/krateoplatformops/plumbing/crdgen`)
**Consumers affected:** core-provider (composition CRD generation), oasgen-provider (RestDefinition CRD generation)
**Author context:** root-caused live against loft-sh/vcluster `v0.36.0` `values.schema.json` (229,311 bytes, 232 `$defs`, ~334 `$refs`).

---

## 1. Summary

crdgen turns a chart's `values.schema.json` into a Kubernetes CRD. Today it does so by
**generating Go structs with kubebuilder markers and running `controller-gen`**. For rich,
`$ref`-heavy schemas this pipeline is **structurally lossy**: it silently substitutes wrong
types and drops whole subtrees. On loft/vcluster 0.36, **~68% of source field-paths are absent
from the generated CRD**, and key nodes are replaced by foreign shapes. The CRD still *installs*
(after the `sanitizeCRD` pass), so the loss is **silent** — the API server prunes CR content
against the wrong schema. This is what caused a live vcluster child to boot empty (the
`Vcluster` CR's entire `spec` — `experimental.deploy`, `controlPlane`, `sync` — was pruned to
`null`).

**Root cause (single mechanism):** every schema node is forced into a **named Go type in one
flat package namespace**, using a **lossy, case-folding name derivation** plus **first-writer-wins
dedup**. Distinct schema concepts alias to the same Go identifier; the wrong body wins.

**Proposed fix:** stop going through Go structs and `controller-gen`. **Transpile JSON Schema
directly into a structural OpenAPI v3 schema**, resolving `$ref` by inlining against a `$defs`
table keyed on the **full JSON pointer** (never a folded identifier), breaking cycles explicitly
with `x-kubernetes-preserve-unknown-fields`, and copying validation keywords straight through.
This eliminates the entire failure class **by construction**, is deterministic, and removes the
`controller-gen` runtime dependency. Constructs a structural CRD schema genuinely cannot express
(true unions, `not`, cross-field `dependentRequired`) degrade **explicitly and locally** to an
open object — never by silently aliasing to a foreign type.

> **plumbing#186 (2026-11-15).** A node with no usable type (`{}`, `items: {}`, a divergent
> `oneOf`/`anyOf`, an `allOf` with no object member) degrades to **any value**:
> `x-kubernetes-preserve-unknown-fields: true` with no `type`. An open object (`type: object`)
> refused every string, number, boolean and list the source accepts. Unresolved `$ref`s and cycle
> breaks stay open objects, with their warnings, and the spec and status roots are always objects.

---

## 2. Evidence (loft/vcluster 0.36, full pipeline)

Generated CRD: 177,533 bytes (v1.10.8). Comparing the generated `spec` schema against the
source (with `$ref`s resolved):

| Path | Source (correct) | Generated CRD (observed) |
|---|---|---|
| `spec.experimental.deploy.vcluster` | `{helm, manifests, manifestsTemplate}` | `{apiVersion, kind, metadata, spec, status}` ← the generator's own **root-Kind** shape |
| `spec.controlPlane` | 13 props (`distro, backingStore, ingress, service, statefulSet, …`) | `{egress, ingress}` ← body of a **different** def (`NetworkPolicyControlPlane`) |
| `spec.controlPlane.statefulSet` | object, 22 props incl. `resources` | **absent** |
| `spec.sync.fromHost` | 16 props incl. `nodes` | `{clusterIssuers}` ← body of `CertManagerSyncFromHost` |
| `spec.sync.toHost` | many props | dropped many; injected foreign `{certificates, issuers}` |
| `spec.controlPlane.ingress` | object (6 props) | **mistyped to `type: array`** |
| `spec.integrations.externalSecrets` | `{enabled, sync, version, webhook}` | dropped subset; injected foreign `{selector}` |

**Quantified:** 7 object subtrees structurally corrupted; 70 properties dropped and 14 foreign
properties injected at the corruption boundaries; 1 object→array mistype. Because each corrupted
node is a whole-subtree loss, a depth-capped reachable-path sweep finds **1098 / 1611 source
field-paths (68%) absent** from the CRD, plus 112 foreign paths. The `{apiVersion, kind,
metadata, spec, status}` 5-tuple **exists nowhere in the source schema** — it is invented by the
generator.

---

## 3. Root cause

The `JSON Schema → Go structs (+kubebuilder markers) → controller-gen → CRD` round-trip forces
**every** schema node to become a **named Go type in a single flat package namespace**. Three
compounding defects follow (all confirmed in the generated Go for this schema):

### 3.1 Identity collapse (lossy naming)
`exportedName` (`coders/support.go:301`) folds non-alphanumerics to `_`, then emits
`ToUpper(first)+ToLower(rest)` per part — **destroying internal capitals**:
`exportedName("ControlPlane") == exportedName("controlPlane") == "Controlplane"`;
`exportedName("VCluster") == "Vcluster"`. Distinct source concepts collapse to one Go identifier.

### 3.2 First-writer-wins dedup, keyed on the collapsed name
Dedup is a `map[string]bool` keyed on the derived name:
- `buildStruct` early-returns if `generatedStructs[typeName]` (`coders/types.go:287`);
- `resolveType`'s `$ref` branch returns the existing name if `generatedStructs[refName]`
  (`types.go:546`) **without checking the shape matches**;
- the inline-object branch, on a taken name, falls back to a **time-seeded random** `Struct_xxx_nnn`
  (`types.go:603`) — 582 such structs exist for this one schema.

So the first type to claim `"Controlplane"` / `"Fromhost"` / `"Vcluster"` wins; every later,
differently-shaped concept with the same collapsed name **silently reuses the foreign body**.
The real type is still emitted (orphaned) under its raw `$defs` key, but nothing references it.

### 3.3 Three disagreeing naming schemes + delegated resolution
Names are derived three incompatible ways: `buildStructForDefs` uses the **raw `$defs` key**
(`types.go:99`); `resolveType` uses **`exportedName(field)`** (`types.go:315`, `:601`); the
top-level Kind scaffold uses **`opts.Kind` verbatim** (`buildEntryItemStructs`, `types.go:218`).
The `experimental.deploy.vcluster` failure is exactly a scaffold/field clash: the field
`vcluster` → `"Vcluster"` collides with the Kind scaffold `"Vcluster"`, producing **two
`type Vcluster struct` in one package (invalid Go)**; controller-gen then binds the field to the
scaffold. Because resolution is **delegated to controller-gen over redeclared types**, the CRD
shape depends on Go's type-checker tie-break — sensitive to the Kind string and generation order
(live: `Kind=VCluster` → correct `{helm,manifests,manifestsTemplate}`; `Kind=Vcluster` → the
corrupt 5-tuple).

**`sanitizeCRD` is not implicated** — it only inserts `type: object` /
`x-kubernetes-preserve-unknown-fields` on already-empty nodes; it never rebinds or drops
properties. It makes the corrupt CRD *installable*, which is what makes the loss silent.

---

## 4. Regression note (the determinism sort)

The four determinism sorts added after v1.10.5 (`types.go:93-97,110-114,306-311`;
`support.go:185-190`) **did not create** this bug — the collisions and corrupt bindings are fully
present at v1.10.5. But they **did change which collision wins**: e.g. `sync.fromHost` at v1.10.5
was **non-deterministic** across runs (sometimes the correct 16-prop body incl. `nodes`,
sometimes `{clusterIssuers}`, sometimes dropped); at v1.10.8 it is **deterministically
`{clusterIssuers}` (wrong) every run**. Reverting the sorts would only restore *nondeterministic
sometimes-correctness* (a CRD whose shape changes run-to-run) — not a fix. The sorts stay; this
redesign removes the collision that made them a liability.

---

## 5. Why not just fix the naming (rejected as the primary fix)

A minimal patch — a globally-unique, shape-aware type-name allocator (reserve the Kind name; key
every type on its full `$def` path; verify shape identity before `$ref`-reuse) — would fix
failures #1 and #2. But it leaves the Go-struct intermediate in place, which is **independently
lossy** and keeps the `controller-gen` dependency:

- **Maps** (`additionalProperties: {schema}`) collapse to `runtime.RawExtension` /
  preserve-unknown (`support.go:281-291`) — the value schema is discarded.
- **Unions** (`oneOf`/`anyOf`) are flattened by `MergeTypes`/mergo — no Go equivalent.
- CRD shape still depends on `controller-gen` + `go build` of generated code (slow, fragile,
  needs the toolchain at runtime).

We would fix the collisions and still ship a lossy generator. The direct transpiler removes the
whole substrate. (A unique-name allocator is still worth doing as an **interim stopgap** if a fix
is needed before the transpiler lands — see §9.)

---

## 6. Design goals

1. **Fidelity:** every construct expressible in a structural CRD schema round-trips losslessly
   from source schema to CRD. No silent field drops, no foreign-type substitution, no mistypes.
2. **Fail loud, locally:** anything a structural CRD genuinely can't express degrades to
   `type: object` + `x-kubernetes-preserve-unknown-fields: true` **at that node only**, and is
   logged — never a silent aliasing that corrupts a sibling/parent.
3. **Determinism:** identical input → byte-identical CRD, guaranteed by a pure schema walk (no
   maps-in-iteration-order, no RNG, no controller-gen tie-break).
4. **Structural-schema conformance by construction:** output already satisfies the API server's
   rules (every object node typed; no `$ref`; no bare empty nodes) — folding in what `sanitizeCRD`
   patches today.
5. **No runtime toolchain:** drop the `controller-gen` + `go build` step from the hot path.
6. **Drop-in:** same `crdgen.Generate(opts) ([]byte, error)` signature; same CRD envelope
   (group/version/kind/scope/subresources/categories/printer columns).

---

## 7. Proposed architecture

### 7.1 Pipeline
```
values.schema.json ─▶ parse (schemas.FromJSONReader)
                   ─▶ build $defs table keyed by JSON pointer  (#/$defs/Foo, #/properties/… )
                   ─▶ transpile(specSchema)  ── pure walk, inline $refs, break cycles ──▶ openAPIV3Schema (spec)
                   ─▶ transpile(statusSchema) ─▶ openAPIV3Schema (status)
                   ─▶ assembleCRD(group, version, kind, scope, categories, subresources, printerColumns,
                                  specSchema, statusSchema)  ─▶ CRD YAML
```
No Go structs, no `controller-gen`. `assembleCRD` builds the `CustomResourceDefinition` envelope
directly (the envelope was never derived from the Go structs anyway — it comes from `Options`).

### 7.2 `$ref` resolution — inline, keyed by JSON pointer
- Build a `refTable map[string]*Schema` keyed on the **full JSON pointer** (`#/$defs/ExperimentalDeployVCluster`,
  `#/$defs/ControlPlane`, …). No `exportedName`, no folding — pointers are already globally unique.
- `transpile(node, path, stack)`:
  - if `node.$ref`: look up the pointer in `refTable`; if on the current `stack` → **cycle**, emit
    the break node (§7.3); else push, `transpile` the target, pop. **Inline** the result (structural
    schemas may not contain `$ref`).
  - else copy the node and recurse into `properties[*]`, `items`, `additionalProperties(schema)`,
    `allOf/anyOf/oneOf[*]` (§7.5).
- Because every node becomes an **anonymous inlined schema**, there is no identifier namespace and
  therefore no collision — failures #1 and #2 cannot occur.

### 7.3 Cycle detection & breaking
Track the set of pointers on the current resolution `stack`. On re-entry to a pointer already on
the stack, stop and emit:
```yaml
type: object
x-kubernetes-preserve-unknown-fields: true
# cycle broken at #/$defs/Foo (self/mutual recursion)
```
This is the only correct finite representation of a recursive schema in a structural CRD, and it
is applied **exactly at the recursion edge** (not to the whole subtree).

### 7.4 Keyword mapping (JSON Schema → structural OpenAPI v3)
| JSON Schema | Structural OpenAPI v3 output |
|---|---|
| `type` (single) | copied |
| `type` as `[T, "null"]` | `type: T` + `nullable: true` |
| `properties` | recursed; parent gets `type: object` |
| `required` | copied |
| `items` | recursed; parent `type: array` |
| `additionalProperties: {schema}` | recursed (**map value schema preserved**) |
| `additionalProperties: true/absent on open node` | `x-kubernetes-preserve-unknown-fields: true` |
| `enum`, `format`, `default`, `pattern`, `minimum/maximum`, `minLength/maxLength`, `multipleOf`, `minItems/maxItems`, `uniqueItems` | copied |
| `description`, `title`, `examples` | copied (`examples[0]` → `example`) |
| `const` | → single-value `enum` (type inferred from the value when undeclared) |
| `nullable` (OAS 3.0) | → `nullable: true` (same as `type: [T,"null"]`) |
| `allOf` | merged (§7.5) |
| `oneOf`/`anyOf` | see §7.5 (structural-safe subset, else degrade) |
| `$ref` | inlined (§7.2); `$defs` indexed at every nesting level (incl. `#/$defs/A/$defs/B`); legacy `#/definitions/…` normalized |
| `$anchor`, `$dynamicAnchor` | indexed as `#<name>`; `$ref`/`$dynamicRef`/`$recursiveRef` resolve against them (dynamic scope not modeled — static fallback) |
| `dependentRequired` | → **CEL** `x-kubernetes-validations`: `!has(self.p) \|\| (has(self.a) && has(self.b))` |
| `not` (scalar `const`/`enum`) | → **CEL** negation (`self != X` / `!(self in […])`); else degrade |
| `if`/`then`/`else` (`required`, and property `const`/`enum`/`minLength`/`maxLength`/`minimum`/`maximum`/exclusive bounds/`minItems`/`maxItems`/`minProperties`/`maxProperties` on a map) | → **CEL** (plumbing#211). A property clause holds where the property is absent (`!has(self.k) \|\| P`), as JSON Schema's `properties`; `has(self.k) && P` where k is required, or defaulted (in the condition). A branch keyword CEL can't express (`pattern`, `format`, `multipleOf`, nested objects) is dropped and **warned**, the rest of the branch enforced; one in the condition degrades the whole if/then/else, **warned** |
| `x-kubernetes-int-or-string` | → typeless int-or-string node |
| `x-kubernetes-{embedded-resource,list-type,list-map-keys,map-type,validations}` | passed through (existing CEL rules preserved) |
| `patternProperties`, `dependentSchemas`, `prefixItems` (tuple), `unevaluatedProperties/Items`, non-trivial `not`/`if` | **degrade** to open object + **warn** (§7.8) |

Generated CEL only references fields that are declared (or on a preserve-unknown object) so every rule compiles; the §8.5 gate validates all CEL, so a bad rule fails generation rather than shipping.

Every object node that has `properties` or an `additionalProperties` schema is emitted with
`type: object` (structural requirement); genuinely open nodes get
`x-kubernetes-preserve-unknown-fields: true`. This subsumes `sanitizeCRD`.

### 7.5 Composition (`allOf` / `oneOf` / `anyOf`)
- **`allOf`**: deep-merge member schemas (properties union, `required` union, keyword intersection
  with conflict → widen). This is what `schemas.AllOf` intends today, done on the inlined tree.
- **`oneOf`/`anyOf`**: Kubernetes structural schemas allow `anyOf`/`oneOf` **only under narrow
  rules** (no `type`/`properties`/etc. that would make the node non-structural at the same level).
  - If members differ only by value constraints (same shape) → keep as `anyOf`/`oneOf` of
    constraint-only members (valid structural).
  - Otherwise (members are different object shapes — a true union) → **degrade** the node to
    `type: object` + preserve-unknown and log. Never flatten-merge divergent shapes (today's silent
    corruption).

### 7.6 Structural-schema conformance
Guaranteed by construction: no `$ref` survives; every `properties`/`items`/`additionalProperties`
parent is typed; no empty `{}` nodes; `nullable` used instead of `type: "null"`. A final
**validation pass** asserts these invariants and fails generation loudly if any are violated (a
generator bug should never again ship a silently-wrong CRD).

### 7.7 Determinism
A pure recursive walk with sorted property iteration and no RNG. Same input → byte-identical
output. (Keeps the intent of the v1.10.6 sorts, without the collision they were papering over.)

### 7.8 Explicit degradation policy
Whenever the transpiler cannot faithfully represent a node (§7.4 unsupported keywords, §7.5 true
unions, §7.3 cycles), it degrades **that node** to `type: object` +
`x-kubernetes-preserve-unknown-fields: true` and records a structured warning
(`path`, `reason`). Degradation is **local and visible** — the opposite of today's non-local
silent aliasing. Callers (core-provider/oasgen) can surface the warnings.

---

## 8. What stays / what changes

- **Stays:** `crdgen.Generate(Options) ([]byte, error)`; CRD envelope assembly from `Options`
  (`Group/Version/Kind/Categories/Managed`); status subresource; the `schemas` parser
  (`FromJSONReader`, `CollectAllDefinitions`).
- **Changes / removed:** `coders` Go-struct emission (`buildStruct*`, `resolveType`,
  `exportedName`-based naming, `generatedStructs`), the `tools.Tidy` + `controller-gen` invocation,
  and `sanitizeCRD` (its job moves into §7.6). The `runtime.RawExtension` map-collapse
  (`support.go:281`) is gone — maps keep their value schema.
- **Printer columns / additional CRD metadata:** if any were derived from kubebuilder markers,
  they move to explicit `Options` fields feeding `assembleCRD`.

---

## 8.5 Compliance assurance — how we guarantee the output is k8s-valid

"100% compliant" is not asserted in prose; it is made **structurally impossible to violate** by
gating on the API server's own code and by closing the transpiler's mapping under validity.

### 8.5.1 "Compliant" == the apiserver's own validators (oracle gate)
Structural-schema validity is *defined* by `k8s.io/apiextensions-apiserver`, so `Generate()` runs
its output through the **same function kube-apiserver runs on CRD admission** and **fails loudly**
if it rejects — the generator cannot return a CRD the apiserver would refuse:

```go
// k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation (validation.go:75, v0.36.2)
func ValidateCustomResourceDefinition(ctx context.Context, obj *apiextensions.CustomResourceDefinition) field.ErrorList
```
An empty `field.ErrorList` means "accepted"; a non-empty list is the rejection reasons. The
apiserver's CRD registry strategy calls this same package, so this is the real admission check run
ahead of time — not a re-implementation. Its `validationOptions` are all on
(`requireStructuralSchema`, `requireOpenAPISchema`, `requireValidPropertyType`,
`requirePrunedDefaults`, `requireAtomicSetType`, `requireMapListKeysMapSetValidation`, CEL env),
and internally (validation.go:312) it calls `structuralschema.NewStructural(...)` +
structural-validation — so the structural check is **subsumed** by this one umbrella call. The
standalone `apiserver/schema.NewStructural` (`convert.go:38`) + `apiserver/schema.ValidateStructural`
(`validation.go:79`) are used only to validate a bare schema node in unit tests.

**Implementation caveat:** `ValidateCustomResourceDefinition` takes the **internal** type
`apiextensions.CustomResourceDefinition`, not the served `apiextensionsv1.CustomResourceDefinition`.
The gate builds the v1 CRD, converts via
`apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition`, then
validates.

The `apiextensions-apiserver` module version is **pinned**; the rules evolve release-to-release
(e.g. CEL via `environment.DefaultCompatibilityVersion()`), so compliance is version-relative and CI
tests against the **min and max k8s versions Krateo supports** (matrix).

### 8.5.2 Compliance is closed under the mapping (by construction)
Every transpiler rule emits either (a) a fragment from the keyword-mapping table, each of which is
provably structural, or (b) a **degrade to `type: object` + `x-kubernetes-preserve-unknown-fields:
true`**, which is *always* structurally valid. There is **no path** that emits an unvalidated
construct. Worst case is explicit, logged fidelity loss — never a non-compliant schema. §8.5.1 is
the belt to this suspenders.

### 8.5.3 Two guarantees, both proven (acceptance ≠ fidelity)
The original bug was "the CRD is *accepted*" masking "the CR is *pruned*." Both are gated:
- **Acceptance:** §8.5.1 library oracle + a **real apiserver** test (envtest / kind,
  `kubectl apply --dry-run=server`) across the supported version matrix.
- **Fidelity / no-prune** (cluster-free, using the apiserver's own pruner): build a sample CR from
  the source schema's defaults/examples, run `apiserver/schema/pruning.Prune(sampleCR, structural)`,
  and assert it is a **no-op** (byte-identical in/out). This is exactly the check that would have
  caught the vcluster empty-spec failure; it is cheap and deterministic.

### 8.5.4 Breadth
- **Corpus** (loft/vcluster 0.36, oasgen OAS-derived, existing goldens) through §8.5.1 + §8.5.3
  every CI run.
- **Property/fuzz:** bounded-random JSON Schemas → transpile → must pass the oracle and the
  no-op-prune check; catches input classes hand-written tests miss.

### 8.5.5 Honest bound
"100%" over *all conceivable* inputs is not provable. What is guaranteed: the generator is gated by
the apiserver's own validator (so it can never emit a rejected CRD), CI proves acceptance +
no-prune fidelity on a real apiserver across the supported version range plus a corpus and a
fuzzer, and anything inexpressible degrades to a provably-valid open object rather than risking
rejection or silent loss.

---

## 9. Rollout / migration

1. **Implement behind a flag** (`CRDGEN_TRANSPILER=direct`), defaulting to the old path, so the two
   can be diffed.
2. **Corpus diff harness:** run both paths over a corpus of real charts (the current golden
   fixtures + loft/vcluster + a few `$ref`-heavy OpenAPI-derived schemas from oasgen) and diff the
   CRDs. For every path present in the source, assert it is present and correctly typed in the
   direct output; catalogue where the old path dropped/mistyped it (regression evidence + proof of
   fix).
3. **Regenerate golden fixtures** from the direct path once diffs are understood; the goldens
   become fidelity assertions (source-path coverage, no foreign properties, structural validity).
4. **Cut over** the default; keep the old path one release behind the flag for escape-hatch, then
   delete `coders` + `controller-gen`.
5. **Consumers:** core-provider and oasgen get *more correct* CRDs. Validate a representative
   composition set (and, for oasgen, a representative RestDefinition/OAS) end-to-end on a kind
   cluster **through the CRD path** (apply a CR, confirm no pruning) before release — the check
   that would have caught this bug.

**Interim stopgap (optional):** if a correct CRD is needed before the transpiler lands, add the
unique-name allocator from §5 (reserve Kind name, key on `$def` path, shape-verify `$ref`-reuse).
It fixes #1/#2 but not the map/union losses — a bridge, not the destination.

---

## 10. Testing strategy

- **Fidelity invariants (property tests):** for a generated CRD, *every* leaf path reachable in the
  source schema (with `$ref`s resolved, cycles excepted) must exist with a compatible type; *no*
  property may appear that isn't in the source (no foreign injection); output must be structurally
  valid (`kubectl apply --dry-run=server` and an internal structural-schema linter).
- **Canonical hard fixture:** loft/vcluster 0.36 — assert `experimental.deploy.vcluster` =
  `{helm, manifests, manifestsTemplate}`, `controlPlane.statefulSet.resources` present,
  `sync.fromHost.nodes` present, and a round-trip CR is **not pruned**.
- **Determinism test:** N runs → identical bytes.
- **Degradation test:** a schema with a true `oneOf` union and a recursive `$ref` produces
  preserve-unknown at exactly those nodes, with warnings, and nothing else degraded.

---

## 11. Risks & mitigations

- **Behavioral change for existing charts.** Mitigate with the corpus diff (§9.2) + one release
  behind a flag; most changes are *added* previously-dropped fields (strictly better).
- **`anyOf`/`oneOf` structural rules are subtle.** Start conservative: only keep constraint-only
  unions; degrade everything else. Tighten later.
- **Effort.** The transpiler is a focused, well-bounded walk (the `schemas` parser already exists);
  smaller than it looks because it *deletes* the `coders`/controller-gen machinery.
- **oasgen coupling.** oasgen derives GVK/version names via `crdgen.NormalizeVersionName` (kept) and
  feeds OAS-derived schemas; include an oasgen schema in the corpus.

---

## 12. Alternatives considered

1. **Unique-name allocator only (§5)** — fixes the collisions, keeps the lossy Go/`controller-gen`
   substrate (maps, unions, runtime dependency). Rejected as the *primary* fix; retained as an
   optional interim stopgap.
2. **Thin permissive wrapper chart** (a braghettos `vcluster` chart with a minimal
   `values.schema.json` → preserve-unknown CRD). Unblocks the vcluster demo without touching
   crdgen, but abandons schema-level validation for the child and doesn't fix crdgen for any other
   rich chart. Explicitly rejected per direction: fix crdgen to surface refs, not paper over it.
3. **Keep controller-gen, post-process the CRD to repair collisions.** Requires re-deriving the
   correct shapes we already lost — strictly harder than transpiling from source. Rejected.
