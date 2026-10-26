package helm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
	"sigs.k8s.io/yaml"
)

// traceparentAnnotation is the W3C trace-context annotation the helm post-renderer
// stamps on every *.krateo.io child resource on each render. It changes every
// reconcile, so it must be neutralized before the change-detection merge (see step
// 4.5 in Reconcile) or it would defeat the apply-if-changed no-churn guarantee.
const traceparentAnnotation = "krateo.io/traceparent"

// tracestateAnnotation is the companion W3C annotation, written by the SAME post-renderer pass
// whenever the trace context carries vendor state (helm/utils/postrenderer.go stamps it alongside
// traceparent when Tracestate is non-empty). It is per-trace and therefore just as volatile, so it
// must be neutralized identically. Step 4.5 originally copied traceparent alone, which left the
// whole no-churn guarantee resting on tracestate happening to be empty.
//
// It IS empty under a bare OTel SDK, so the gap was latent rather than live; a vendor propagator or
// any sampling state in tracestate makes it real. Note the cdc's digest path already treats both as
// volatile and names them both (processor.stripTraceContext), and this is the third comparison path
// over the same objects — a sibling path not inheriting the rule is exactly what caused the
// revision-4463 churn incident.
const tracestateAnnotation = "krateo.io/tracestate"

// volatileTraceAnnotations is the closed set of per-reconcile trace-context annotations neutralized
// in step 4.5. Deliberately an explicit list and NOT a krateo.io/trace* prefix match: a prefix would
// silently absorb any future annotation in that namespace that IS declared desired state, and a
// neutralized field never heals on drift — so membership has to be opted into rather than inferred.
var volatileTraceAnnotations = []string{traceparentAnnotation, tracestateAnnotation}

// Reconcile performs a fork-free, self-healing "apply-if-changed" reconcile of
// a helm release using ONLY helm's exported Go API.
//
// The flow:
//  1. GetRelease -> the stored release. Its .Manifest is the CURRENT (last
//     applied) manifest. If the release does not exist we cannot self-heal an
//     absent release; we delegate to Upgrade (Install:true) and report Changed.
//  2. Render the TARGET manifest via a server-side dry-run Upgrade. This runs
//     the exact render pipeline (post-renderer, template funcs, lookups,
//     server validation) and returns Release.Manifest WITHOUT writing a
//     revision or running hooks.
//  3. Build helm kube ResourceLists from the stored (current) and rendered
//     (target) manifests using KubeClient.Build.
//  4. GET each target object's live state (the BEFORE snapshot for step 6) and
//     capture its live krateo.io/traceparent. Absent (IsNotFound) => will be
//     created. Then neutralize two volatile, non-semantic sources of per-render
//     diff so they cannot force a revision: copy the live traceparent onto the
//     target (4.5) and fold Secret stringData into data as the apiserver does (4.6).
//  5. KubeClient.UpdateThreeWayMerge(current, target, force) — helm's 3-way merge.
//     This CREATES resources deleted out-of-band and PATCHES drifted ones,
//     converging the cluster. The merge refreshes every target Info's Object in
//     place to its POST-merge server state (patch branch: Refresh; no-op: Get).
//  6. changed := any Created || any Deleted || any surviving target whose content
//     actually changed. We compare each child's BEFORE (step 4 live) and AFTER
//     (step 5 refreshed) state with write-volatile and server-owned fields
//     stripped (see strippedJSON/semanticallyChanged) — NOT a resourceVersion
//     delta and NOT len(Result.Updated), both of which over-count server-owned
//     no-op re-applies (a Namespace's apiserver-restored metadata.name, a
//     helm-forced managed-by) and churned a revision every cycle at steady state.
//  7. If changed, run a real Upgrade ONCE to write the revision and run hooks
//     with correct ordering/delete-policy (delegated to action.Upgrade rather
//     than reimplementing hook handling). Step 5 already converged the cluster,
//     so this Upgrade is a near-no-op re-apply.
//  8. If NOT changed, return Changed:false — no Releases write, no hooks.
func (c *client) Reconcile(ctx context.Context, releaseName, chartRef string, cfg *helmconfig.UpgradeConfig) (*helmconfig.ReconcileResult, error) {
	if cfg == nil {
		cfg = &helmconfig.UpgradeConfig{}
	}
	if cfg.ActionConfig == nil {
		cfg.ActionConfig = &helmconfig.ActionConfig{}
	}

	// 1. Stored (current) release.
	stored, err := c.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
	if err != nil {
		return nil, fmt.Errorf("reconcile: getting stored release: %w", err)
	}
	if stored == nil {
		// No release yet: there is nothing to self-heal against. Perform a real
		// install-or-upgrade and report it as a change. The caller (cdc Create
		// path) normally handles this, but we keep Reconcile total.
		installCfg := *cfg
		installCfg.Install = true
		rel, err := c.Upgrade(ctx, releaseName, chartRef, &installCfg)
		if err != nil {
			return nil, fmt.Errorf("reconcile: initial upgrade (no stored release): %w", err)
		}
		return &helmconfig.ReconcileResult{Changed: true, Release: rel}, nil
	}
	currentManifest := stored.Manifest

	// 1.5. Repair the stored manifest against GVKs the cluster no longer serves. When a child's
	// CompositionDefinition is bumped out-of-band PAST the umbrella's pin, the old served CRD
	// version is pruned; the stored manifest still references it, so helm's OWN current-manifest
	// Build inside the step-2 dry-run Upgrade fails hard ("unable to build kubernetes objects from
	// current release manifest: no matches for kind ... ensure CRDs are installed first") before
	// the reconcile can run — a deadlock a controller restart does NOT clear. We drop the
	// unmappable entries (using the SAME REST mapper helm uses, so we drop exactly what it can't
	// build) and persist the repaired release, so the render proceeds. Each dropped child is live
	// at the NEW version and is re-adopted by step 4.7 below; the next real Upgrade (step 7)
	// rewrites the manifest with the served versions, after which this is a no-op.
	if repaired, dropped, rerr := c.dropUnservedFromStoredManifest(ctx, cfg, releaseName, currentManifest); rerr != nil {
		return nil, fmt.Errorf("reconcile: repairing stored manifest: %w", rerr)
	} else if len(dropped) > 0 {
		currentManifest = repaired
	}

	// 2. Render the target manifest via a server-side dry-run Upgrade (no
	//    revision written, no hooks run). This is ONLY how we obtain the
	//    rendered target cheaply through the public API; it is NOT an
	//    up-to-date gate.
	dryCfg := *cfg
	dryActionCfg := *cfg.ActionConfig
	dryActionCfg.DryRun = helmconfig.DryRunServer
	dryCfg.ActionConfig = &dryActionCfg
	rendered, err := c.Upgrade(ctx, releaseName, chartRef, &dryCfg)
	if err != nil {
		return nil, fmt.Errorf("reconcile: rendering target (dry-run): %w", err)
	}
	targetManifest := rendered.Manifest

	// 3. Build current + target resource lists via helm's own kube client.
	actionConfig, err := c.newActionConfig(ctx, c.namespace, c.restConfig, cfg.ActionConfig)
	if err != nil {
		return nil, fmt.Errorf("reconcile: building action config: %w", err)
	}
	kc := actionConfig.KubeClient

	current, err := kc.Build(strings.NewReader(currentManifest), false)
	if err != nil {
		return nil, fmt.Errorf("reconcile: building current resource list: %w", err)
	}
	target, err := kc.Build(strings.NewReader(targetManifest), true)
	if err != nil {
		return nil, fmt.Errorf("reconcile: building target resource list: %w", err)
	}

	// 4. GET each target object's live state (for the change-detection diff in step 5) and
	//    capture its current trace-context annotations (for the copy in step 4.5).
	//    Missing (IsNotFound) => absent, will be created by the merge.
	absent := make(map[string]bool, len(target))
	liveTrace := make(map[string]map[string]string, len(target))
	liveObj := make(map[string]runtime.Object, len(target))
	for _, info := range target {
		key := infoKey(info)
		helper := resource.NewHelper(info.Client, info.Mapping)
		live, gerr := helper.Get(info.Namespace, info.Name)
		if gerr != nil {
			// Treat any get failure (typically NotFound) as absent; the merge will
			// create it and surface a real error if the create fails.
			absent[key] = true
			continue
		}
		liveObj[key] = live
		for _, ann := range volatileTraceAnnotations {
			v := annotationOf(live, ann)
			if v == "" {
				continue
			}
			if liveTrace[key] == nil {
				liveTrace[key] = make(map[string]string, len(volatileTraceAnnotations))
			}
			liveTrace[key][ann] = v
		}
	}

	// 4.5. Neutralize the volatile trace-context stamps. The helm post-renderer stamps a FRESH
	// krateo.io/traceparent — and krateo.io/tracestate when the trace carries vendor state — on
	// every *.krateo.io child on each render (trace-context propagation). Those values change every
	// reconcile, so without this the three-way merge below would patch them every cycle — an RV
	// bump that reads as a real "change" (step 6), forcing a helm revision + hooks even though
	// nothing else moved (the exact per-minute revision churn this reconcile exists to kill;
	// observed on krateo.io-child compositions like snowplow's authn ServiceAccount and the
	// installer umbrella). We copy the LIVE child's existing values onto the rendered target so the
	// annotations contribute no diff: when they are the ONLY difference the merge patch is empty and
	// helm skips the write entirely (RV stable -> changed=false -> no revision, no hooks). Genuine
	// drift in every OTHER field still heals. A real change re-stamps fresh values through the
	// actual Upgrade in step 7. Freshly-created children (no live value) keep the target stamp.
	//
	// Per annotation rather than all-or-nothing, so a child that carries traceparent but not yet
	// tracestate still has its traceparent neutralized. Each key it lacks costs one revision the
	// first time that key is written, then stays stable — the same one-shot as the very first
	// traceparent, not recurring churn.
	for _, info := range target {
		key := infoKey(info)
		if absent[key] {
			continue
		}
		for ann, v := range liveTrace[key] {
			setAnnotation(info.Object, ann, v)
		}
	}

	// 4.6. Normalize Secret stringData -> data. A chart that writes a Secret via
	// stringData never converges under an apply loop: the apiserver base64-encodes
	// stringData into data and DROPS stringData, so the next render re-adds stringData
	// while live only has data — an eternal diff the three-way merge patches every cycle
	// (revision churn even when the secret VALUE is unchanged; observed on the installer
	// umbrella's jwt-sign-key). We fold stringData into data (exactly as the apiserver
	// does: data[k] = base64(stringData[k]), stringData wins on key collisions) on the
	// rendered target so it matches the stored representation and contributes no diff when
	// the value is stable. A real value change still differs in data -> changed=true. This
	// runs on every target (a create-time secret is equally valid expressed as data).
	for _, info := range target {
		normalizeSecretStringData(info.Object)
	}

	// 4.7. Adopt live-existing target resources that are MISSING from `current`. A child's
	// CRD-version migration (composition.krateo.io/v1-2-2 -> v1-3-2 after a component version
	// bump) leaves the stored manifest either pinning the OLD version — which kc.Build can no
	// longer map once that version is pruned from the CRD, so it silently drops the entry
	// (the builder runs ContinueOnError) — or missing the child outright. Either way `current`
	// ends up WITHOUT a resource that is (a) rendered in `target` and (b) already live. helm's
	// Update then reaches `original.Get(info) == nil` for that live object and returns
	// "no <kind> with the name <name> found" (pkg/kube/client.go), failing the whole reconcile
	// and wedging it forever — the stored manifest never advances past the migration, so every
	// future reconcile re-hits the same missing entry. (Observed live: the installer umbrella
	// lost KrateoFrontend/frontend from its stored manifest across the frontend v1-2-2 -> v1-3-2
	// bump while the CR stayed live and healthy.)
	//
	// NOTE this is the INVERSE of a delete problem: helm matches original<->target on
	// group+kind+name+namespace, VERSION-INDEPENDENT (isMatchingInfo), so an old-GVK entry that
	// DOES survive in `current` still matches its new-GVK target and is patched, never deleted —
	// dropping such an entry (an earlier approach) would itself CREATE this error. The only
	// failure mode is a live target with NO original at all, so we ADOPT: inject the live object
	// as the missing `current` entry, and helm three-way-merges (patches) it instead of erroring.
	// Guarded on live-present AND genuinely-absent-from-current, so it can only convert an
	// otherwise-certain "no <kind> found" into a normal patch — never alters a converging case.
	for _, tinfo := range target {
		if absent[infoKey(tinfo)] {
			continue // live absent -> helm's createResource path handles it
		}
		if current.Get(tinfo) != nil {
			continue // a matching original already exists (any version) -> normal patch/no-op
		}
		// Use the TARGET object (not the live one) as the adopted `original`. helm's
		// createPatch computes CreateThreeWayJSONMergePatch(original, target, live): a key
		// present in `original` but absent from `target` is DELETED. The rendered target does
		// not carry helm's own ownership metadata (app.kubernetes.io/managed-by,
		// meta.helm.sh/release-*) — the apiserver/helm add it at apply time — so injecting the
		// LIVE object as `original` would make the merge strip that metadata, and the follow-up
		// real Upgrade (step 7) would then reject the now-unowned resource ("cannot be imported").
		// With original == target the merge has zero deletions: it reconciles live to target and
		// PRESERVES every live-only field (ownership labels, controller-written fields). That is
		// exactly adopt semantics — "helm last applied this target" — and it heals genuine drift
		// in target-declared fields all the same.
		adopted := *tinfo
		adopted.Object = tinfo.Object.DeepCopyObject()
		current = append(current, &adopted)
	}

	// 5. Self-healing merge: creates children deleted out-of-band, reverts field drift. This is the
	// only mutation. UpdateThreeWayMerge (NOT Update) so UNSTRUCTURED/CR children get helm's
	// three-way-with-live merge; the concrete *kube.Client implements the compat-split
	// kube.InterfaceThreeWayMerge, so type-assert to reach it (no fork), falling back to the 2-way
	// Update if a KubeClient ever doesn't implement it. helm refreshes every target Info's Object to
	// its POST-merge server state (patch branch: Refresh; no-op branch: Get) — step 6 relies on that.
	tw, ok := kc.(kube.InterfaceThreeWayMerge)
	var res *kube.Result
	if ok {
		res, err = tw.UpdateThreeWayMerge(current, target, cfg.Force)
	} else {
		res, err = kc.Update(current, target, cfg.Force)
	}
	if err != nil {
		return nil, fmt.Errorf("reconcile: kube update (self-heal apply): %w", err)
	}

	// 6. Decide whether the server-PERSISTED state actually changed — not merely whether the merge
	// bumped a resourceVersion. The old RV-delta over-counted: helm's merge patches (and bumps RV
	// on) fields the apiserver then no-ops or re-defaults — removing a Namespace's server-owned
	// kubernetes.io/metadata.name (auto-restored) or app.kubernetes.io/managed-by (helm-forced) —
	// so a composition with such a child churned a helm revision every cycle at steady state
	// (portal's demo-system Namespace, the umbrella's managed-by). Instead we compare each child's
	// content BEFORE (liveObj, step 4) and AFTER (info.Object, refreshed by the merge above) with
	// write-volatile + server-owned metadata stripped (see strippedJSON): identical => the merge
	// fought a server-owned field and the apiserver reverted it — a no-op, not a real change;
	// different => a genuine create/heal. This uses helm's OWN merge (no separate dry-run, so no
	// patch-type mismatch) and immunizes against ANY server-owned/defaulted field. It also subsumes
	// the per-field neutralizations (traceparent copy in 4.5, stringData fold in 4.6) — those keep
	// the before/after identical for their children. created/deleted come from the merge result.
	created := len(res.Created)
	deleted := len(res.Deleted)
	patched := 0
	for _, info := range target {
		key := infoKey(info)
		if absent[key] {
			continue // counted under res.Created
		}
		changedChild, cerr := semanticallyChanged(liveObj[key], info.Object)
		if cerr != nil {
			// Undetermined -> fail safe by treating it as a change.
			patched++
			continue
		}
		if changedChild {
			patched++
		}
	}

	changed := created > 0 || deleted > 0 || patched > 0

	result := &helmconfig.ReconcileResult{
		Changed:        changed,
		Created:        created,
		Deleted:        deleted,
		PatchedUpdated: patched,
		Release:        stored,
	}

	if !changed {
		// Steady state: the merge only touched server-owned fields the apiserver reverted. No
		// revision, no hooks.
		return result, nil
	}

	// 7. A real change happened. Run ONE real Upgrade to persist a revision and run hooks with
	// correct ordering + delete-policy. Step 5 already converged the cluster, so this is a near-no-op.
	rel, err := c.Upgrade(ctx, releaseName, chartRef, cfg)
	if err != nil {
		return result, fmt.Errorf("reconcile: real upgrade after detected change: %w", err)
	}
	result.Release = rel
	return result, nil
}

// childWouldChange reports whether applying the rendered target to this live child would actually
// change the SERVER-PERSISTED state (a real change), as opposed to a server-normalized no-op that
// merely bumps resourceVersion.
//
// Cheap pre-filter: the three-way JSON merge patch (stored-original, target, live). An empty patch
// means the declared state already matches and live-only fields are preserved -> no change, skip
// the API round-trip. A NON-empty patch might still be a server no-op (e.g. it removes the
// apiserver-owned kubernetes.io/metadata.name label, or a Namespace's spec.finalizers, which the
// server re-adds/preserves), so we confirm the way `kubectl diff` does: a SERVER-SIDE APPLY dry-run
// of the target (not the raw merge patch — that would delete live-only fields the server keeps).
// SSA runs the apiserver's defaulting/admission/field-ownership merge, so the returned object is
// exactly what WOULD be stored; comparing it to live (ignoring purely volatile metadata) tells us
// semanticallyChanged reports whether a child's content actually changed across the merge — i.e.
// whether `before` (the live object read in step 4) and `after` (the same object refreshed by the
// merge in step 5) differ once write-volatile and server-owned metadata are stripped (see
// strippedJSON). A merge that only re-added an apiserver-owned field (kubernetes.io/metadata.name)
// or re-forced app.kubernetes.io/managed-by leaves before == after here (both already carried the
// server value), so it reads as no-change even though the resourceVersion bumped; a real create or
// drift-heal leaves them different.
func semanticallyChanged(before, after runtime.Object) (bool, error) {
	beforeJSON, err := strippedJSON(before)
	if err != nil {
		return false, fmt.Errorf("strip before: %w", err)
	}
	afterJSON, err := strippedJSON(after)
	if err != nil {
		return false, fmt.Errorf("strip after: %w", err)
	}
	patch, err := jsonpatch.CreateMergePatch(beforeJSON, afterJSON)
	if err != nil {
		return false, fmt.Errorf("compare: %w", err)
	}
	return !isEmptyPatch(patch), nil
}

// isEmptyPatch reports whether a JSON merge patch is a no-op ("" or "{}").
func isEmptyPatch(patch []byte) bool {
	s := strings.TrimSpace(string(patch))
	return s == "" || s == "{}"
}

// strippedJSON marshals obj to JSON with never-drift fields removed (see semanticallyChanged):
// volatile server metadata (resourceVersion, generation, uid, creationTimestamp, managedFields),
// helm-owned ownership metadata (the three-way merge preserves these regardless), and status.
func strippedJSON(obj runtime.Object) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]interface{}); ok {
		for _, k := range []string{"resourceVersion", "generation", "uid", "creationTimestamp", "managedFields", "selfLink"} {
			delete(md, k)
		}
		if ann, ok := md["annotations"].(map[string]interface{}); ok {
			delete(ann, "meta.helm.sh/release-name")
			delete(ann, "meta.helm.sh/release-namespace")
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
		}
		if lbl, ok := md["labels"].(map[string]interface{}); ok {
			delete(lbl, "app.kubernetes.io/managed-by")
		}
	}
	return json.Marshal(m)
}

// infoKey uniquely identifies a resource.Info across the current/target lists.
func infoKey(info *resource.Info) string {
	gvk := info.Object.GetObjectKind().GroupVersionKind()
	return fmt.Sprintf("%s/%s/%s/%s", gvk.GroupVersion().String(), gvk.Kind, info.Namespace, info.Name)
}

// filterUnmappableManifestDocs splits a helm manifest into documents and drops any whose GVK
// the given RESTMapper cannot resolve — i.e. an apiVersion the cluster no longer serves. It
// returns the rejoined manifest of the KEPT docs and the list of dropped GVKs (as
// "group/version, Kind=..." strings). Documents whose header cannot be parsed are kept, never
// dropped blindly. Pure (no I/O) so it is unit-testable with a fake mapper; the persisting
// wrapper is dropUnservedFromStoredManifest.
func filterUnmappableManifestDocs(manifest string, mapper apimeta.RESTMapper) (string, []string) {
	kept := make([]string, 0)
	var dropped []string
	for _, doc := range releaseutil.SplitManifests(manifest) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var head struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil || head.Kind == "" || head.APIVersion == "" {
			kept = append(kept, doc) // unparseable header -> keep (never drop blindly)
			continue
		}
		gv, err := schema.ParseGroupVersion(head.APIVersion)
		if err != nil {
			kept = append(kept, doc)
			continue
		}
		if _, err := mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: head.Kind}, gv.Version); err != nil {
			dropped = append(dropped, gv.WithKind(head.Kind).String())
			continue
		}
		kept = append(kept, doc)
	}
	if len(dropped) == 0 {
		return manifest, nil
	}
	return strings.Join(kept, "\n---\n"), dropped
}

// dropUnservedFromStoredManifest removes resources whose GVK the cluster no longer serves from
// the stored release manifest and, if any were dropped, PERSISTS the repaired manifest back to
// the release store so helm's own current-manifest Build (in the step-2 dry-run Upgrade)
// succeeds. It builds an action config with the SAME arguments Upgrade uses, so it shares the
// REST mapper (drops precisely what helm cannot build) and the release storage (the k8s
// Secrets that Upgrade reads). See the step-1.5 comment in Reconcile for why. Returns the
// (possibly unchanged) manifest and the dropped GVKs.
func (c *client) dropUnservedFromStoredManifest(ctx context.Context, cfg *helmconfig.UpgradeConfig, releaseName, manifest string) (string, []string, error) {
	actionConfig, err := c.newActionConfig(ctx, c.namespace, c.restConfig, cfg.ActionConfig)
	if err != nil {
		return manifest, nil, fmt.Errorf("building action config: %w", err)
	}
	mapper, err := actionConfig.RESTClientGetter.ToRESTMapper()
	if err != nil {
		return manifest, nil, fmt.Errorf("rest mapper: %w", err)
	}
	// Repair the SAME revision helm's Upgrade builds its "current" manifest from (see
	// selectCurrentForBuild / helm upgrade.go prepareUpgrade) and filter THAT revision's manifest.
	// Blindly repairing Last() was the bug: when a `failed`/`superseded` revision sits above the
	// `deployed` one, helm builds Deployed while we fixed Last, so the "no matches for kind" wedge
	// survived (observed on an Installer release whose `deployed` revision still pinned a pruned
	// child apiVersion, under a `failed` revision on top).
	last, err := actionConfig.Releases.Last(releaseName)
	if err != nil {
		return manifest, nil, fmt.Errorf("load release for repair: %w", err)
	}
	rel := selectCurrentForBuild(last, func() (*release.Release, error) {
		return actionConfig.Releases.Deployed(releaseName)
	})
	repaired, dropped := filterUnmappableManifestDocs(rel.Manifest, mapper)
	if len(dropped) == 0 {
		return manifest, nil, nil
	}
	rel.Manifest = repaired
	if err := actionConfig.Releases.Update(rel); err != nil {
		return manifest, dropped, fmt.Errorf("persist repaired release: %w", err)
	}
	return repaired, dropped, nil
}

// selectCurrentForBuild returns the revision whose manifest helm's Upgrade builds as the "current"
// state, mirroring helm upgrade.go prepareUpgrade: the last revision when it is Deployed, else the
// last Deployed revision, else (none Deployed) the last. deployed is called lazily so it is only
// queried when the last revision is not itself Deployed.
func selectCurrentForBuild(last *release.Release, deployed func() (*release.Release, error)) *release.Release {
	if last != nil && last.Info != nil && last.Info.Status == release.StatusDeployed {
		return last
	}
	if dep, err := deployed(); err == nil && dep != nil {
		return dep
	}
	return last
}

// resourceVersionOf extracts metadata.resourceVersion from a runtime.Object
// (typed or unstructured) via apimachinery's meta accessor. Returns "" when it
// cannot be read.
func resourceVersionOf(obj runtime.Object) string {
	acc, err := apimeta.Accessor(obj)
	if err != nil {
		return ""
	}
	return acc.GetResourceVersion()
}

// annotationOf reads a single metadata annotation from a runtime.Object (typed or
// unstructured) via apimachinery's meta accessor. Returns "" when absent/unreadable.
func annotationOf(obj runtime.Object, key string) string {
	acc, err := apimeta.Accessor(obj)
	if err != nil {
		return ""
	}
	ann := acc.GetAnnotations()
	if ann == nil {
		return ""
	}
	return ann[key]
}

// setAnnotation sets a single metadata annotation on a runtime.Object (typed or
// unstructured) in place via apimachinery's meta accessor. No-op if the object has
// no accessible metadata. Mutating the target Info's Object here is reflected in the
// patch helm computes at merge time (helm re-encodes info.Object to build the diff).
func setAnnotation(obj runtime.Object, key, val string) {
	acc, err := apimeta.Accessor(obj)
	if err != nil {
		return
	}
	ann := acc.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[key] = val
	acc.SetAnnotations(ann)
}

// normalizeSecretStringData folds a core/v1 Secret's stringData into data in place,
// mirroring the apiserver's write-time behavior: each stringData[k] is base64-encoded
// into data[k] (stringData wins on collision) and stringData is removed. This keeps a
// stringData-authored Secret from reading as perpetual drift against its stored (data)
// representation. helm's kube Build yields *unstructured.Unstructured for every object,
// so a non-Secret / non-unstructured object is a no-op.
func normalizeSecretStringData(obj runtime.Object) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	if u.GetKind() != "Secret" || u.GetAPIVersion() != "v1" {
		return
	}
	stringData, found, err := unstructured.NestedStringMap(u.Object, "stringData")
	if err != nil || !found || len(stringData) == 0 {
		return
	}
	data, _, _ := unstructured.NestedStringMap(u.Object, "data")
	if data == nil {
		data = map[string]string{}
	}
	for k, v := range stringData {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	di := make(map[string]interface{}, len(data))
	for k, v := range data {
		di[k] = v
	}
	_ = unstructured.SetNestedMap(u.Object, di, "data")
	unstructured.RemoveNestedField(u.Object, "stringData")
}
