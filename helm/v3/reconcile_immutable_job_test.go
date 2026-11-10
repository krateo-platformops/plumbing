//go:build integration
// +build integration

package helm

// core-provider#403 on a real apiserver. Before the fix (plumbing v1.35.0, measured on kind):
//   A. only a plain Job's pod template changes  -> err=nil changed=false, no revision, Job never updated
//   B. Job template + a Deployment change       -> one immutable error, then err=nil on a FAILED rev 2
//   C. control: Deployment replicas -1          -> the error on every reconcile
// Decided (Diego, 2026-11-10): a chart-changed template on a live Job is reported (Synced=False), and
// a failed latest revision is not converged (the upgrade is retried and its error returned).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const immutableJobChartDir = "testdata/charts/immutable-job"

type jobProbe struct {
	t       *testing.T
	cs      *kubernetes.Clientset
	cli     *client
	ns      string
	release string
	chart   string
}

func newJobProbe(ctx context.Context, t *testing.T, c *envconf.Config, suffix string) *jobProbe {
	cs, err := kubernetes.NewForConfig(c.Client().RESTConfig())
	require.NoError(t, err)
	ns := fmt.Sprintf("%s-%s", namespace, suffix)
	_, err = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	require.NoError(t, err)
	cli, err := NewClient(c.Client().RESTConfig(), WithNamespace(ns),
		WithLogger(func(format string, v ...interface{}) { t.Logf("[helm] "+format, v...) }))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	p := &jobProbe{t: t, cs: cs, cli: cli, ns: ns, release: "probe", chart: chartArchiveURL(t, immutableJobChartDir)}
	_, err = cli.Install(ctx, p.release, p.chart, &helmconfig.InstallConfig{ActionConfig: &helmconfig.ActionConfig{TakeOwnership: true}})
	require.NoError(t, err)
	return p
}

func (p *jobProbe) reconcile(ctx context.Context, values map[string]interface{}) (*helmconfig.ReconcileResult, error) {
	return p.cli.Reconcile(ctx, p.release, p.chart, &helmconfig.UpgradeConfig{
		ActionConfig: &helmconfig.ActionConfig{TakeOwnership: true, Values: values},
	})
}

func (p *jobProbe) liveJobCmd() string {
	j, err := p.cs.BatchV1().Jobs(p.ns).Get(context.Background(), "probe-plain-job", metav1.GetOptions{})
	if err != nil {
		return "<absent>"
	}
	return strings.Join(j.Spec.Template.Spec.Containers[0].Command, " ")
}

func (p *jobProbe) deployEnv() string {
	d, err := p.cs.AppsV1().Deployments(p.ns).Get(context.Background(), "probe-deploy", metav1.GetOptions{})
	require.NoError(p.t, err)
	if e := d.Spec.Template.Spec.Containers[0].Env; len(e) > 0 {
		return e[0].Value
	}
	return ""
}

func (p *jobProbe) latest() (int, helmconfig.Status) {
	rel, err := p.cli.GetRelease(context.Background(), p.release, &helmconfig.GetConfig{})
	require.NoError(p.t, err)
	require.NotNil(p.t, rel)
	return rel.Revision, rel.Status
}

func (p *jobProbe) log(step string, res *helmconfig.ReconcileResult, err error) {
	rev, st := p.latest()
	detail := "-"
	if res != nil {
		detail = fmt.Sprintf("changed=%v jobs=%v retriedFailed=%v", res.Changed, res.ChangedJobTemplates, res.RetriedFailedRelease)
	}
	p.t.Logf("PROBE403 %s step=%s err=%v %s revisions=%d latest=rev %d %s liveJob=%q env=%q",
		p.ns, step, err, detail, helmRevisionCount(p.t, p.cs, p.ns, p.release), rev, st, p.liveJobCmd(), p.deployEnv())
}

func TestReconcileReportsImmutableJobAndFailedRevision(t *testing.T) {
	f := features.New("core-provider#403").
		Assess("A: a Job-only template change is reported, not silently skipped", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			p := newJobProbe(ctx, t, c, "a")
			for _, step := range []string{"r1", "r2"} {
				res, err := p.reconcile(ctx, map[string]interface{}{"jobMessage": "v2"})
				p.log(step, res, err)
				require.Error(t, err, "%s: a release that cannot be applied must not read as converged", step)
				assert.True(t, errors.Is(err, helmconfig.ErrJobTemplateChanged), "%s: err = %v, want ErrJobTemplateChanged", step, err)
				require.NotNil(t, res)
				assert.Equal(t, []string{p.ns + "/probe-plain-job"}, res.ChangedJobTemplates)
				assert.Equal(t, "sh -c echo v1", p.liveJobCmd(), "the live Job is never patched (it cannot be)")
				assert.Equal(t, 1, helmRevisionCount(t, p.cs, p.ns, p.release), "no revision for a release that was not applied")
			}
			return ctx
		}).
		Assess("B: the other children still converge, and the error persists", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			p := newJobProbe(ctx, t, c, "b")
			vals := map[string]interface{}{"jobMessage": "v2", "deployEnv": "changed"}
			for _, step := range []string{"r1", "r2"} {
				res, err := p.reconcile(ctx, vals)
				p.log(step, res, err)
				require.Error(t, err, "%s: before the fix r2 returned nil on a failed revision", step)
				assert.True(t, errors.Is(err, helmconfig.ErrJobTemplateChanged), "%s: err = %v", step, err)
				assert.Equal(t, "changed", p.deployEnv(), "%s: the Deployment change must still be applied (#276)", step)
			}
			return ctx
		}).
		Assess("C: control, a non-immutable patch failure is reported every time", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			p := newJobProbe(ctx, t, c, "c")
			for _, step := range []string{"r1", "r2"} {
				res, err := p.reconcile(ctx, map[string]interface{}{"replicas": -1})
				p.log(step, res, err)
				require.Error(t, err)
				assert.False(t, errors.Is(err, helmconfig.ErrJobTemplateChanged), "the Job did not change; err = %v", err)
				assert.Contains(t, err.Error(), "spec.replicas")
			}
			return ctx
		}).
		Assess("D: a failed latest revision is retried, not read as converged, and recovers", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			p := newJobProbe(ctx, t, c, "d")
			vals := map[string]interface{}{"jobMessage": "v2"}
			// A failed revision, made the way a plain helm upgrade makes one.
			_, uerr := p.cli.Upgrade(ctx, p.release, p.chart, &helmconfig.UpgradeConfig{
				ActionConfig: &helmconfig.ActionConfig{TakeOwnership: true, Values: vals},
			})
			require.Error(t, uerr, "setup: the upgrade must fail on the immutable Job template")
			rev, st := p.latest()
			require.Equal(t, helmconfig.StatusFailed, st, "setup: latest revision %d must be failed", rev)

			// Stored (failed rev 2) and target agree on the Job, so only the failed-revision rule can
			// keep this from reading as converged.
			res, err := p.reconcile(ctx, vals)
			p.log("failed-r1", res, err)
			require.Error(t, err, "a failed latest revision must not read as converged")
			require.NotNil(t, res)
			assert.True(t, res.RetriedFailedRelease)
			assert.Contains(t, err.Error(), "whose latest revision")
			assert.False(t, errors.Is(err, helmconfig.ErrJobTemplateChanged), "stored and target agree on the Job; err = %v", err)

			// Recovery: once the Job is gone, the retried upgrade succeeds.
			bg := metav1.DeletePropagationBackground
			require.NoError(t, p.cs.BatchV1().Jobs(p.ns).Delete(ctx, "probe-plain-job", metav1.DeleteOptions{PropagationPolicy: &bg}))
			require.Eventually(t, func() bool { return p.liveJobCmd() == "<absent>" }, 2*time.Minute, time.Second)
			res, err = p.reconcile(ctx, vals)
			p.log("recovered", res, err)
			require.NoError(t, err)
			_, st = p.latest()
			assert.Equal(t, helmconfig.StatusDeployed, st)
			assert.Equal(t, "sh -c echo v2", p.liveJobCmd())

			// And then it is steady: no retry once the latest revision is deployed.
			res, err = p.reconcile(ctx, vals)
			p.log("steady", res, err)
			require.NoError(t, err)
			assert.False(t, res.RetriedFailedRelease)
			assert.False(t, res.Changed)
			return ctx
		})
	testenv.Test(t, f.Feature())
}
