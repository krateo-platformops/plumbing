//go:build integration
// +build integration

package helm

// plumbing#106 on a real apiserver: helm's update result can't tell a failed write from a done one
// (it appends to Updated "regardless"), so the verdicts read the refreshed object instead. This pins
// that helm behaviour at plumbing's helm version: a helm bump must keep it green (see go.mod).

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

const partialApplyChartDir = "testdata/charts/partial-apply"

func TestReconcilePartialApplyVerdicts(t *testing.T) {
	f := features.New("plumbing#106 partial apply").
		Assess("one of three patches fails: one updateFailed, two specChanged, counts agree", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cs, err := kubernetes.NewForConfig(c.Client().RESTConfig())
			require.NoError(t, err)
			ns := fmt.Sprintf("%s-partial", namespace)
			_, err = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			require.NoError(t, err)
			cli, err := NewClient(c.Client().RESTConfig(), WithNamespace(ns),
				WithLogger(func(format string, v ...interface{}) { t.Logf("[helm] "+format, v...) }))
			require.NoError(t, err)
			t.Cleanup(func() { _ = cli.Close() })
			chart := chartArchiveURL(t, partialApplyChartDir)
			const release = "pa"
			_, err = cli.Install(ctx, release, chart, &helmconfig.InstallConfig{ActionConfig: &helmconfig.ActionConfig{TakeOwnership: true}})
			require.NoError(t, err)
			frozen, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, release+"-frozen", metav1.GetOptions{})
			require.NoError(t, err)
			revBefore := helmRevisionCount(t, cs, ns, release)

			res, err := cli.Reconcile(ctx, release, chart, &helmconfig.UpgradeConfig{
				ActionConfig: &helmconfig.ActionConfig{TakeOwnership: true, Values: map[string]interface{}{"color": "red"}},
			})
			require.Error(t, err, "the immutable ConfigMap's patch must fail")
			require.NotNil(t, res)
			t.Logf("created=%d patched=%d objects=%+v notAttempted=%d", res.Created, res.PatchedUpdated, res.Objects, res.ObjectsNotAttempted)
			assert.True(t, res.AppliedPartially)
			assert.True(t, res.ObjectsComplete)
			assert.Zero(t, res.ObjectsNotAttempted, "an update failure doesn't stop helm, so every child was attempted")

			got := map[string]helmconfig.ObjectVerdict{}
			for _, v := range res.Objects {
				got[v.Name] = v
			}
			require.Len(t, res.Objects, 3, "%+v", res.Objects)
			for _, name := range []string{release + "-a", release + "-b"} {
				assert.Equal(t, helmconfig.ChangeSpec, got[name].Change, name)
				assert.Equal(t, "patch", got[name].Verb, name)
			}
			assert.Equal(t, helmconfig.ChangeUpdateFailed, got[release+"-frozen"].Change)
			assert.Equal(t, "patch", got[release+"-frozen"].Verb)
			assert.Equal(t, string(frozen.UID), got[release+"-frozen"].UID, "a failed patch names the live object")

			// The counts come from the same predicate: the failed patch is not a change.
			assert.Equal(t, 0, res.Created)
			assert.Equal(t, 2, res.PatchedUpdated, "only the confirmed patches count")
			assert.Len(t, res.ChangedObjects, 2, "the failed child is not listed as changed: %v", res.ChangedObjects)
			assert.Equal(t, revBefore, helmRevisionCount(t, cs, ns, release), "a partial apply writes no revision")

			live, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, release+"-frozen", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, "blue", live.Data["color"], "the immutable ConfigMap is unchanged")
			return ctx
		}).Feature()
	testenv.Test(t, f)
}
