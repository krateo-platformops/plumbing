//go:build integration
// +build integration

package helm

import (
	"context"
	"fmt"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const driftHooksChartDir = "testdata/charts/drift-hooks"

// helmRevisionCount returns the number of stored helm release revisions
// (secret-backed) for releaseName. A no-op reconcile must NOT increase it.
func helmRevisionCount(t *testing.T, cs *kubernetes.Clientset, ns, releaseName string) int {
	t.Helper()
	list, err := cs.CoreV1().Secrets(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("name=%s,owner=helm", releaseName),
	})
	require.NoError(t, err)
	return len(list.Items)
}

// hookFireCount returns the number of hook-rev-* ConfigMaps, i.e. how many
// times the pre-upgrade hook has fired for this release.
func hookFireCount(t *testing.T, cs *kubernetes.Clientset, ns string) int {
	t.Helper()
	list, err := cs.CoreV1().ConfigMaps(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: "drift-hooks/hook=pre-upgrade",
	})
	require.NoError(t, err)
	return len(list.Items)
}

func liveColor(t *testing.T, cs *kubernetes.Clientset, ns, name string) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return cm.Data["color"]
}

func liveTraceparent(t *testing.T, cs *kubernetes.Clientset, ns, name string) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return cm.Annotations["krateo.io/traceparent"]
}

func liveTracestate(t *testing.T, cs *kubernetes.Clientset, ns, name string) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return cm.Annotations["krateo.io/tracestate"]
}

func liveSecretVal(t *testing.T, cs *kubernetes.Clientset, ns, name, key string) string {
	t.Helper()
	s, err := cs.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return string(s.Data[key]) // clientset decodes data (base64) into raw bytes
}

func liveNamespaceLabel(t *testing.T, cs *kubernetes.Clientset, name, key string) string {
	t.Helper()
	n, err := cs.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return n.Labels[key]
}

// TestControllerReconcile proves the fork-free apply-if-changed Reconcile on a
// real kind cluster: (c) FIELD DRIFT self-heal and (d) HOOKS ONLY ON CHANGE.
func TestControllerReconcile(t *testing.T) {
	f := features.New("Reconcile apply-if-changed").
		Assess("field-drift self-heal and hooks-only-on-change", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cs, err := kubernetes.NewForConfig(c.Client().RESTConfig())
			require.NoError(t, err)

			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithLogger(func(format string, v ...interface{}) {
					fmt.Println(fmt.Sprintf(format, v...))
				}),
				WithNamespace(namespace),
			)
			require.NoError(t, err)
			defer cli.Close()

			chartURL := chartArchiveURL(t, driftHooksChartDir)
			releaseName := "drift-hooks"
			cmName := releaseName + "-config"

			upCfg := func() *helmconfig.UpgradeConfig {
				return &helmconfig.UpgradeConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
						Values: map[string]interface{}{
							"color": "blue",
						},
					},
				}
			}

			// --- Initial install (revision 1). ---
			rel, err := cli.Install(ctx, releaseName, chartURL, &helmconfig.InstallConfig{ActionConfig: upCfg().ActionConfig})
			require.NoError(t, err)
			require.Equal(t, 1, rel.Revision)
			t.Cleanup(func() {
				_ = cli.Uninstall(context.Background(), releaseName, &helmconfig.UninstallConfig{IgnoreNotFound: true})
			})

			require.Equal(t, "blue", liveColor(t, cs, namespace, cmName))
			revAfterInstall := helmRevisionCount(t, cs, namespace, releaseName)
			hooksAfterInstall := hookFireCount(t, cs, namespace)
			t.Logf("[install] revisions=%d hookFires=%d color=%s", revAfterInstall, hooksAfterInstall, liveColor(t, cs, namespace, cmName))

			// --- Steady-state reconcile (no drift): must be a no-op. ---
			res, err := cli.Reconcile(ctx, releaseName, chartURL, upCfg())
			require.NoError(t, err)
			assert.False(t, res.Changed, "steady-state reconcile must not report a change")
			revSteady := helmRevisionCount(t, cs, namespace, releaseName)
			hooksSteady := hookFireCount(t, cs, namespace)
			t.Logf("[steady] Changed=%v revisions=%d (was %d) hookFires=%d (was %d)",
				res.Changed, revSteady, revAfterInstall, hooksSteady, hooksAfterInstall)
			assert.Equal(t, revAfterInstall, revSteady, "no-op reconcile must not add a helm revision")
			assert.Equal(t, hooksAfterInstall, hooksSteady, "no-op reconcile must not fire the pre-upgrade hook")

			// =========================================================
			// (c) FIELD DRIFT: mutate data.color out-of-band, reconcile must patch it back.
			// =========================================================
			cm, err := cs.CoreV1().ConfigMaps(namespace).Get(ctx, cmName, metav1.GetOptions{})
			require.NoError(t, err)
			t.Logf("[drift] color BEFORE out-of-band mutation = %q", cm.Data["color"])
			cm.Data["color"] = "red-DRIFTED"
			_, err = cs.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.Equal(t, "red-DRIFTED", liveColor(t, cs, namespace, cmName))
			t.Logf("[drift] color AFTER out-of-band mutation = %q", liveColor(t, cs, namespace, cmName))

			revBeforeHeal := helmRevisionCount(t, cs, namespace, releaseName)
			hooksBeforeHeal := hookFireCount(t, cs, namespace)

			res, err = cli.Reconcile(ctx, releaseName, chartURL, upCfg())
			require.NoError(t, err)
			assert.True(t, res.Changed, "reconcile must detect+heal the field drift")
			assert.GreaterOrEqual(t, res.PatchedUpdated, 1, "the drifted ConfigMap must be patched")
			healedColor := liveColor(t, cs, namespace, cmName)
			revAfterHeal := helmRevisionCount(t, cs, namespace, releaseName)
			hooksAfterHeal := hookFireCount(t, cs, namespace)
			t.Logf("[heal] Changed=%v patched=%d color=%q revisions=%d (was %d) hookFires=%d (was %d)",
				res.Changed, res.PatchedUpdated, healedColor, revAfterHeal, revBeforeHeal, hooksAfterHeal, hooksBeforeHeal)

			assert.Equal(t, "blue", healedColor, "field drift must be self-healed back to declared value")
			assert.Equal(t, revBeforeHeal+1, revAfterHeal, "a real change must bump the helm revision exactly once")
			// (d) the real change fired the pre-upgrade hook exactly once.
			assert.Equal(t, hooksBeforeHeal+1, hooksAfterHeal, "a real change must fire the pre-upgrade hook exactly once")

			// --- Following steady-state reconcile: no further bump, no further hook. ---
			res, err = cli.Reconcile(ctx, releaseName, chartURL, upCfg())
			require.NoError(t, err)
			assert.False(t, res.Changed, "post-heal steady-state reconcile must be a no-op")
			revSettled := helmRevisionCount(t, cs, namespace, releaseName)
			hooksSettled := hookFireCount(t, cs, namespace)
			t.Logf("[settled] Changed=%v color=%q revisions=%d (was %d) hookFires=%d (was %d)",
				res.Changed, liveColor(t, cs, namespace, cmName), revSettled, revAfterHeal, hooksSettled, hooksAfterHeal)
			assert.Equal(t, revAfterHeal, revSettled, "steady-state after heal must not add a revision")
			assert.Equal(t, hooksAfterHeal, hooksSettled, "steady-state after heal must not fire the hook again")

			// =========================================================
			// (d) HOOKS ONLY ON CHANGE — explicit second real change.
			// A no-op loop (5x) must not fire the hook; a genuine value change must.
			// =========================================================
			hooksPreLoop := hookFireCount(t, cs, namespace)
			revPreLoop := helmRevisionCount(t, cs, namespace, releaseName)
			for i := 0; i < 5; i++ {
				r, err := cli.Reconcile(ctx, releaseName, chartURL, upCfg())
				require.NoError(t, err)
				assert.False(t, r.Changed, "no-op reconcile #%d must not change", i)
			}
			hooksPostLoop := hookFireCount(t, cs, namespace)
			revPostLoop := helmRevisionCount(t, cs, namespace, releaseName)
			t.Logf("[hook-loop] after 5 no-op reconciles: hookFires %d->%d revisions %d->%d",
				hooksPreLoop, hooksPostLoop, revPreLoop, revPostLoop)
			assert.Equal(t, hooksPreLoop, hooksPostLoop, "5 no-op reconciles must not fire the hook")
			assert.Equal(t, revPreLoop, revPostLoop, "5 no-op reconciles must not add revisions")

			// Now a genuine declared-value change (color blue->green): hook must fire once.
			changeCfg := &helmconfig.UpgradeConfig{
				ActionConfig: &helmconfig.ActionConfig{
					TakeOwnership: true,
					Values:        map[string]interface{}{"color": "green"},
				},
			}
			res, err = cli.Reconcile(ctx, releaseName, chartURL, changeCfg)
			require.NoError(t, err)
			assert.True(t, res.Changed, "declared value change must be detected")
			hooksPostChange := hookFireCount(t, cs, namespace)
			t.Logf("[hook-change] color now %q hookFires %d->%d",
				liveColor(t, cs, namespace, cmName), hooksPostLoop, hooksPostChange)
			assert.Equal(t, "green", liveColor(t, cs, namespace, cmName))
			assert.Equal(t, hooksPostLoop+1, hooksPostChange, "a genuine change must fire the hook exactly once")

			// =========================================================
			// (e) TRACEPARENT NO-CHURN — a change confined to the volatile
			// krateo.io/traceparent annotation must be a no-op. This is the exact
			// interaction that made snowplow + the installer umbrella keep churning:
			// the post-renderer stamps a fresh traceparent on every *.krateo.io child
			// each render, and without step 4.5 the three-way merge patches it every
			// cycle (revision++). The chart's ConfigMap now carries
			// krateo.io/traceparent={{ .Values.traceparent }} to simulate that stamp.
			// =========================================================
			cfgTrace := func(color, tp, ts string) *helmconfig.UpgradeConfig {
				return &helmconfig.UpgradeConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
						Values:        map[string]interface{}{"color": color, "traceparent": tp, "tracestate": ts},
					},
				}
			}
			// Holds tracestate fixed so this block varies traceparent ALONE.
			cfgTP := func(color, tp string) *helmconfig.UpgradeConfig {
				return cfgTrace(color, tp, "vendor=fixed")
			}

			// Establish a baseline traceparent (real change: "" -> TP-A).
			res, err = cli.Reconcile(ctx, releaseName, chartURL, cfgTP("green", "TP-A"))
			require.NoError(t, err)
			assert.True(t, res.Changed, "adding the first traceparent value is a real change")
			assert.Equal(t, "TP-A", liveTraceparent(t, cs, namespace, cmName))
			revBaseTP := helmRevisionCount(t, cs, namespace, releaseName)
			hooksBaseTP := hookFireCount(t, cs, namespace)

			// Now render a DIFFERENT traceparent each cycle with NO other change.
			// Every one must be a no-op: no revision, no hook, and the live value must
			// stay at the baseline (step 4.5 copies live TP-A onto the target).
			for i, tp := range []string{"TP-B", "TP-C", "TP-D", "TP-E", "TP-F"} {
				r, err := cli.Reconcile(ctx, releaseName, chartURL, cfgTP("green", tp))
				require.NoError(t, err)
				assert.False(t, r.Changed, "traceparent-only reconcile #%d (render=%s) must be a no-op", i, tp)
			}
			revAfterTP := helmRevisionCount(t, cs, namespace, releaseName)
			hooksAfterTP := hookFireCount(t, cs, namespace)
			liveTP := liveTraceparent(t, cs, namespace, cmName)
			t.Logf("[traceparent] after 5 differing-traceparent reconciles: revisions %d->%d hookFires %d->%d liveTraceparent=%q",
				revBaseTP, revAfterTP, hooksBaseTP, hooksAfterTP, liveTP)
			assert.Equal(t, revBaseTP, revAfterTP, "traceparent-only churn must NOT add helm revisions")
			assert.Equal(t, hooksBaseTP, hooksAfterTP, "traceparent-only churn must NOT fire hooks")
			assert.Equal(t, "TP-A", liveTP, "live traceparent must stay at baseline (never patched by the volatile render)")

			// Contrast: a real change alongside a new traceparent must still go through
			// (and the actual upgrade re-stamps traceparent to the new value).
			res, err = cli.Reconcile(ctx, releaseName, chartURL, cfgTP("yellow", "TP-REAL"))
			require.NoError(t, err)
			assert.True(t, res.Changed, "a real data change must be detected even amid a traceparent change")
			assert.Equal(t, "yellow", liveColor(t, cs, namespace, cmName))
			assert.Equal(t, "TP-REAL", liveTraceparent(t, cs, namespace, cmName), "a real upgrade re-stamps the fresh traceparent")
			assert.Equal(t, revAfterTP+1, helmRevisionCount(t, cs, namespace, releaseName), "the real change bumps the revision exactly once")

			// =========================================================
			// (e2) TRACESTATE NO-CHURN — the same guarantee for the companion stamp.
			// Step 4.5 originally copied krateo.io/traceparent alone, so the whole
			// no-churn property rested on tracestate happening to be empty (it is,
			// under a bare OTel SDK). A vendor propagator or any sampling state makes
			// it non-empty and every cycle writes a revision and re-runs hooks.
			//
			// This block holds traceparent FIXED and varies tracestate alone, which is
			// what makes it a test of the tracestate path rather than a repeat of (e):
			// with only the traceparent copy in place, these reconciles are changes.
			// =========================================================
			cfgTS := func(ts string) *helmconfig.UpgradeConfig {
				return cfgTrace("yellow", "TP-REAL", ts)
			}

			// The baseline is INHERITED, not established here: every reconcile in block (e) rendered
			// tracestate as "vendor=fixed" to hold it constant while traceparent varied, so the live
			// ConfigMap already carries that value. An earlier version of this block asserted that
			// the first tracestate render was a real change, which failed — correctly — because step
			// 4.5 neutralized it against the inherited live value. The code was right and the
			// assertion was wrong.
			baselineTS := liveTracestate(t, cs, namespace, cmName)
			require.Equal(t, "vendor=fixed", baselineTS, "block (e) should have left tracestate at its fixed value")
			revBaseTS := helmRevisionCount(t, cs, namespace, releaseName)
			hooksBaseTS := hookFireCount(t, cs, namespace)

			for i, ts := range []string{"TS-B", "TS-C", "TS-D", "TS-E", "TS-F"} {
				r, err := cli.Reconcile(ctx, releaseName, chartURL, cfgTS(ts))
				require.NoError(t, err)
				assert.False(t, r.Changed, "tracestate-only reconcile #%d (render=%s) must be a no-op", i, ts)
			}
			revAfterTS := helmRevisionCount(t, cs, namespace, releaseName)
			hooksAfterTS := hookFireCount(t, cs, namespace)
			liveTS := liveTracestate(t, cs, namespace, cmName)
			t.Logf("[tracestate] after 5 differing-tracestate reconciles: revisions %d->%d hookFires %d->%d liveTracestate=%q",
				revBaseTS, revAfterTS, hooksBaseTS, hooksAfterTS, liveTS)
			assert.Equal(t, revBaseTS, revAfterTS, "tracestate-only churn must NOT add helm revisions")
			assert.Equal(t, hooksBaseTS, hooksAfterTS, "tracestate-only churn must NOT fire hooks")
			assert.Equal(t, baselineTS, liveTS, "live tracestate must stay at baseline (never patched by the volatile render)")

			// Both stamps changing at once, still with no declared change: the two
			// neutralizations must compose rather than one masking the other.
			resBoth, err := cli.Reconcile(ctx, releaseName, chartURL, cfgTrace("yellow", "TP-BOTH", "TS-BOTH"))
			require.NoError(t, err)
			assert.False(t, resBoth.Changed, "traceparent AND tracestate both changing is still a no-op")
			assert.Equal(t, revAfterTS, helmRevisionCount(t, cs, namespace, releaseName), "neither stamp adds a revision")

			// Contrast, so the block cannot pass by neutralizing everything: a real
			// data change alongside both fresh stamps must still go through.
			resReal, err := cli.Reconcile(ctx, releaseName, chartURL, cfgTrace("teal", "TP-BOTH", "TS-BOTH"))
			require.NoError(t, err)
			assert.True(t, resReal.Changed, "a real data change must still be detected amid both stamps changing")
			assert.Equal(t, "teal", liveColor(t, cs, namespace, cmName))
			assert.Equal(t, "TS-BOTH", liveTracestate(t, cs, namespace, cmName), "a real upgrade re-stamps the fresh tracestate")

			// =========================================================
			// (f) STRINGDATA NO-CHURN — a Secret authored via stringData must not
			// churn once its value is stable. The apiserver folds stringData into data
			// and drops stringData; without Reconcile step 4.6 the render re-adds
			// stringData every cycle and the merge patches it forever (the installer
			// umbrella's jwt-sign-key). The drift-hooks chart's Secret uses
			// stringData.token={{ .Values.token }}.
			// =========================================================
			secretName := releaseName + "-secret"
			cfgTok := func(color, tok string) *helmconfig.UpgradeConfig {
				return &helmconfig.UpgradeConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
						Values:        map[string]interface{}{"color": color, "token": tok},
					},
				}
			}

			// One reconcile to settle the secret at a known token (may be a real change
			// since earlier configs didn't set token -> default "seed-token").
			_, err = cli.Reconcile(ctx, releaseName, chartURL, cfgTok("yellow", "tok-STABLE"))
			require.NoError(t, err)
			require.Equal(t, "tok-STABLE", liveSecretVal(t, cs, namespace, secretName, "token"))
			revBaseSD := helmRevisionCount(t, cs, namespace, releaseName)
			hooksBaseSD := hookFireCount(t, cs, namespace)

			// Same stringData value, repeated: every reconcile must be a no-op despite the
			// render carrying stringData while live carries only data.
			for i := 0; i < 5; i++ {
				r, err := cli.Reconcile(ctx, releaseName, chartURL, cfgTok("yellow", "tok-STABLE"))
				require.NoError(t, err)
				assert.False(t, r.Changed, "stable-stringData reconcile #%d must be a no-op", i)
			}
			revAfterSD := helmRevisionCount(t, cs, namespace, releaseName)
			hooksAfterSD := hookFireCount(t, cs, namespace)
			t.Logf("[stringData] after 5 stable-stringData reconciles: revisions %d->%d hookFires %d->%d secretToken=%q",
				revBaseSD, revAfterSD, hooksBaseSD, hooksAfterSD, liveSecretVal(t, cs, namespace, secretName, "token"))
			assert.Equal(t, revBaseSD, revAfterSD, "stable stringData must NOT add helm revisions")
			assert.Equal(t, hooksBaseSD, hooksAfterSD, "stable stringData must NOT fire hooks")
			assert.Equal(t, "tok-STABLE", liveSecretVal(t, cs, namespace, secretName, "token"))

			// A genuine secret value change must still be detected and applied.
			res, err = cli.Reconcile(ctx, releaseName, chartURL, cfgTok("yellow", "tok-CHANGED"))
			require.NoError(t, err)
			assert.True(t, res.Changed, "a real stringData value change must be detected")
			assert.Equal(t, "tok-CHANGED", liveSecretVal(t, cs, namespace, secretName, "token"), "the new secret value must be applied")
			assert.Equal(t, revAfterSD+1, helmRevisionCount(t, cs, namespace, releaseName), "the real secret change bumps the revision exactly once")

			// =========================================================
			// (g) SERVER-MANAGED FIELD NO-CHURN — the chart renders a Namespace WITHOUT the
			// kubernetes.io/metadata.name label; the apiserver auto-stamps it. The old RV-delta
			// change-detection fought that server-managed label every cycle (portal's demo-system
			// Namespace); the server-side-diff must treat the apiserver re-adding it as no-change.
			nsName := releaseName + "-demo"
			// The apiserver has stamped kubernetes.io/metadata.name by now; confirm it's there and
			// NOT in the render.
			require.Equal(t, nsName, liveNamespaceLabel(t, cs, nsName, "kubernetes.io/metadata.name"),
				"apiserver should auto-stamp kubernetes.io/metadata.name")
			revBaseNS := helmRevisionCount(t, cs, namespace, releaseName)
			hooksBaseNS := hookFireCount(t, cs, namespace)
			for i := 0; i < 5; i++ {
				r, err := cli.Reconcile(ctx, releaseName, chartURL, cfgTok("yellow", "tok-CHANGED"))
				require.NoError(t, err)
				assert.False(t, r.Changed, "server-managed-field reconcile #%d must be a no-op (apiserver auto-label is not real drift)", i)
			}
			revAfterNS := helmRevisionCount(t, cs, namespace, releaseName)
			hooksAfterNS := hookFireCount(t, cs, namespace)
			t.Logf("[server-managed] after 5 reconciles with a Namespace carrying an apiserver auto-label: revisions %d->%d hookFires %d->%d",
				revBaseNS, revAfterNS, hooksBaseNS, hooksAfterNS)
			assert.Equal(t, revBaseNS, revAfterNS, "an apiserver-managed field must NOT add helm revisions")
			assert.Equal(t, hooksBaseNS, hooksAfterNS, "an apiserver-managed field must NOT fire hooks")

			// =========================================================
			// (h) ADOPT MISSING-FROM-CURRENT — the exact live wedge. A resource that is RENDERED
			// (target) and LIVE but ABSENT from the stored manifest (current). A child's CRD-version
			// migration drops the old-GVK entry from `current` (kc.Build can't map the pruned version)
			// while the CR stays live, so helm's Update hits original.Get()==nil and errors
			// "no <kind> with the name <name> found" — wedging the reconcile forever (installer-cv2zwx7v
			// lost KrateoFrontend/frontend exactly this way across the frontend v1-2-2 -> v1-3-2 bump).
			// Reconcile step 4.7 must inject the live object into `current` and ADOPT it.
			adoptedName := releaseName + "-adopted"
			// Pre-create the ConfigMap LIVE, out-of-band, with a value the chart does NOT render, so the
			// adopt produces a real patch. adopted was never rendered before -> absent from the manifest.
			// It MUST carry this release's helm ownership metadata (managed-by=Helm + release-name/
			// namespace), mirroring the live wedge: the umbrella's KrateoFrontend/frontend was helm-owned
			// (release=installer-cv2zwx7v) yet missing from the stored manifest. Without ownership, helm's
			// dry-run render (Reconcile step 2) rejects it at the import gate BEFORE step 4.7 can adopt it —
			// which is a DIFFERENT failure than the one under test.
			_, err = cs.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      adoptedName,
					Namespace: namespace,
					Labels:    map[string]string{"app.kubernetes.io/managed-by": "Helm"},
					Annotations: map[string]string{
						"meta.helm.sh/release-name":      releaseName,
						"meta.helm.sh/release-namespace": namespace,
					},
				},
				Data: map[string]string{"note": "pre-existing-live"},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			// Sanity: it is live but NOT in the stored release manifest.
			relBefore, err := cli.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
			require.NoError(t, err)
			require.NotContains(t, relBefore.Manifest, adoptedName,
				"precondition: the adopted ConfigMap must be absent from the stored manifest")

			cfgAdopt := func(note string) *helmconfig.UpgradeConfig {
				return &helmconfig.UpgradeConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
						// Keep every other dimension at its current stable value so the ONLY change
						// is the adopted ConfigMap appearing in the render.
						Values: map[string]interface{}{
							"color": "yellow", "token": "tok-CHANGED",
							"adopted": true, "adoptedNote": note,
						},
					},
				}
			}

			// The adopt reconcile MUST NOT error (this is the whole fix) and must patch the
			// live object to the rendered value.
			res, err = cli.Reconcile(ctx, releaseName, chartURL, cfgAdopt("adopted-render"))
			require.NoError(t, err, "adopting a live target missing from the stored manifest must NOT error")
			assert.True(t, res.Changed, "adopting + patching the pre-existing ConfigMap is a real change")
			adoptedNote := func() string {
				cm, e := cs.CoreV1().ConfigMaps(namespace).Get(ctx, adoptedName, metav1.GetOptions{})
				require.NoError(t, e)
				return cm.Data["note"]
			}
			assert.Equal(t, "adopted-render", adoptedNote(), "the adopted resource must be patched to the rendered value")
			relAfter, err := cli.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
			require.NoError(t, err)
			assert.Contains(t, relAfter.Manifest, adoptedName,
				"after adoption the resource must be captured in the stored manifest")
			t.Logf("[adopt] Changed=%v note=%q now-in-manifest=%v", res.Changed, adoptedNote(), true)

			// Follow-up steady reconcile: now tracked -> no-op, no error.
			res, err = cli.Reconcile(ctx, releaseName, chartURL, cfgAdopt("adopted-render"))
			require.NoError(t, err)
			assert.False(t, res.Changed, "after adoption the resource is tracked -> steady no-op")

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}
