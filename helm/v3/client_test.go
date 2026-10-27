//go:build integration
// +build integration

package helm

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xenv "github.com/krateo-platformops/plumbing/env"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"
	"sigs.k8s.io/kustomize/kyaml/kio"
)

var (
	testenv     env.Environment
	clusterName string
	namespace   string
)

const duplicateResourcesChartDir = "testdata/charts/duplicate-resources"

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	// A FRESH namespace per run, because the Finish hooks below are deliberately disabled so a
	// failed run leaves its objects available for post-mortem inspection. With a fixed name that
	// made the suite runnable exactly ONCE: the second invocation died in setup with
	// `namespaces "helm-test-system" already exists` and reported FAIL in ~14s with no failing
	// test names, which reads like a broken build rather than a dirty cluster. Nobody hit it
	// while nothing ran this suite (#51, finding 1).
	//
	// Deleting the leftover namespace at setup was the obvious fix and it does not work: measured
	// here, a used namespace sat in Terminating for 30+ minutes with every condition reporting
	// success ("All content successfully removed", "All content-preserving finalizers finished")
	// and never completed. Waiting on it just moves the failure to a timeout, and the timeout has
	// no correct value — it depends on the previous run's debris.
	//
	// A unique name needs no cleanup, no waiting and no timeout, and it isolates runs from each
	// other's releases as a side effect: revision counts and hook-fire counts start from zero
	// rather than from whatever the last run left. Old namespaces accumulate in a reused local
	// cluster, which is harmless and visible; CI runners are ephemeral.
	namespace = fmt.Sprintf("helm-test-system-%d", time.Now().UnixNano()%1e9)
	clusterName = "helm-test"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		envfuncs.CreateNamespace(namespace),
	).Finish(
	// envfuncs.DeleteNamespace(namespace),
	// envfuncs.DestroyCluster(clusterName),
	)

	os.Exit(testenv.Run(m))
}

func cloneChartFixture(t *testing.T, srcDir string, mutate func(relPath string, data []byte) []byte) string {
	t.Helper()

	dstDir := t.TempDir()

	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		targetPath := filepath.Join(dstDir, relPath)
		if info.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		if mutate != nil {
			data = mutate(relPath, data)
		}

		return os.WriteFile(targetPath, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy chart fixture: %v", err)
	}

	return dstDir
}

func chartArchiveURL(t *testing.T, chartDir string) string {
	t.Helper()

	ch, err := loader.LoadDir(chartDir)
	if err != nil {
		t.Fatalf("load chart from %s: %v", chartDir, err)
	}

	archiveDir := t.TempDir()
	archivePath, err := chartutil.Save(ch, archiveDir)
	if err != nil {
		t.Fatalf("package chart from %s: %v", chartDir, err)
	}

	server := httptest.NewServer(http.FileServer(http.Dir(archiveDir)))
	t.Cleanup(server.Close)

	return server.URL + "/" + filepath.Base(archivePath)
}

func duplicateResourcesChartURL(t *testing.T) string {
	t.Helper()

	return chartArchiveURL(t, duplicateResourcesChartDir)
}

func cleanDuplicateResourcesChartURL(t *testing.T) string {
	t.Helper()

	cleanChartDir := cloneChartFixture(t, duplicateResourcesChartDir, func(relPath string, data []byte) []byte {
		if relPath == filepath.Join("templates", "deployment-2.yaml") {
			return []byte(strings.ReplaceAll(string(data), "{{ .Chart.Name }}", "{{ .Chart.Name }}-2"))
		}
		return data
	})

	return chartArchiveURL(t, cleanChartDir)
}

func TestDuplicateResourcesInstallAndUpgrade(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Duplicate resources on install and upgrade").
		Assess("Install the duplicate chart and then upgrade into it", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithNamespace(namespace),
			)
			require.NoError(t, err)
			defer cli.Close()

			values := map[string]interface{}{
				"replicaCount": 1,
				"image": map[string]interface{}{
					"repository": "nginx",
					"pullPolicy": "IfNotPresent",
					"tag":        "1.14.2",
				},
			}

			duplicateChartURL := duplicateResourcesChartURL(t)
			seedChartURL := cleanDuplicateResourcesChartURL(t)

			failedInstallRelease := "duplicate-resources-install"
			defer func() {
				_ = cli.Uninstall(ctx, failedInstallRelease, &helmconfig.UninstallConfig{IgnoreNotFound: true})
			}()

			_, err = cli.Install(ctx, failedInstallRelease, duplicateChartURL, &helmconfig.InstallConfig{
				ActionConfig: &helmconfig.ActionConfig{
					TakeOwnership: true,
					Values:        values,
				},
			})
			t.Log("Attempted to install duplicate resources chart", err)
			assert.Error(t, err, "installing the duplicate chart should fail")

			seedRelease := "duplicate-resources-seed"
			rel, err := cli.Install(ctx, seedRelease, seedChartURL, &helmconfig.InstallConfig{
				ActionConfig: &helmconfig.ActionConfig{
					TakeOwnership: true,
					Values:        values,
				},
			})
			require.NoError(t, err)
			require.NotNil(t, rel)
			defer func() {
				_ = cli.Uninstall(ctx, seedRelease, &helmconfig.UninstallConfig{IgnoreNotFound: true})
			}()

			_, err = cli.Upgrade(ctx, seedRelease, duplicateChartURL, &helmconfig.UpgradeConfig{
				ActionConfig: &helmconfig.ActionConfig{
					Values: values,
				},
			})
			assert.Error(t, err, "upgrading to the duplicate chart should fail")

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

func TestInstallAndUninstall_OCI(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Install and Uninstall OCI Chart").
		Assess("Install and Uninstall nginx from OCI", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// 1. Initialize Client
			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithNamespace(namespace),
			)
			assert.Nil(t, err)

			assert.Nil(t, err)
			defer cli.Close()

			// 3. Define Chart Details - Using official Bitnami OCI registry
			chartRef := "oci://registry-1.docker.io/bitnamicharts/nginx"
			releaseName := "test-release-oci"

			t.Logf("Attempting to install chart: %s", chartRef)

			// 4. Run Install
			rel, err := cli.Install(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
					},
				},
			)
			assert.Nil(t, err)
			assert.Equal(t, releaseName, rel.Name)
			assert.Equal(t, "deployed", string(rel.Status))

			t.Logf("Successfully installed release: %s (v%d)", rel.Name, rel.Revision)

			// 5. Run Uninstall
			t.Logf("Attempting to uninstall release: %s", releaseName)
			err = cli.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{})
			assert.Nil(t, err)

			t.Log("Successfully uninstalled release")

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

func TestUpgrade(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Upgrade Release").
		Assess("Install and Upgrade nginx chart", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithLogger(func(format string, v ...interface{}) {
					fmt.Println(fmt.Sprintf(format, v...))
				}),
				WithNamespace(namespace),
			)
			assert.Nil(t, err)
			defer cli.Close()

			chartRef := "oci://registry-1.docker.io/bitnamicharts/nginx"
			releaseName := "test-upgrade"

			// Install initial version
			t.Log("Installing initial release")
			rel, err := cli.Install(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
						Values: map[string]interface{}{
							"replicaCount": 1,
						},
					},
				},
			)
			assert.Nil(t, err)
			assert.Equal(t, releaseName, rel.Name)
			assert.Equal(t, 1, rel.Revision)

			// Upgrade the release
			t.Log("Upgrading release")
			rel, err = cli.Upgrade(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.UpgradeConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
						Values: map[string]interface{}{
							"replicaCount": 2,
						},
					},
				},
			)
			assert.Nil(t, err)
			assert.Equal(t, releaseName, rel.Name)
			assert.Equal(t, 2, rel.Revision)

			// Cleanup
			err = cli.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{})
			assert.Nil(t, err)

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

func TestRollback(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Rollback Release").
		Assess("Install, Upgrade, and Rollback nginx chart", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithNamespace(namespace),
			)
			assert.Nil(t, err)
			defer cli.Close()

			chartRef := "oci://registry-1.docker.io/bitnamicharts/nginx"
			releaseName := "test-rollback"

			// Install initial version
			t.Log("Installing initial release")
			rel, err := cli.Install(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
						Values: map[string]interface{}{
							"replicaCount": 1,
						},
					},
				},
			)
			assert.Nil(t, err)
			assert.Equal(t, 1, rel.Revision)

			// Upgrade the release
			t.Log("Upgrading release")
			rel, err = cli.Upgrade(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.UpgradeConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
						Values: map[string]interface{}{
							"replicaCount": 3,
						},
					},
				},
			)
			assert.Nil(t, err)
			assert.Equal(t, 2, rel.Revision)

			// Rollback to revision 1
			t.Log("Rolling back to revision 1")
			rel, err = cli.Rollback(
				ctx,
				releaseName,
				&helmconfig.RollbackConfig{
					ReleaseVersion: 1,
				},
			)
			assert.Nil(t, err)
			assert.Equal(t, 3, rel.Revision) // Rollback creates a new revision

			// Cleanup
			err = cli.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{})
			assert.Nil(t, err)

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

func TestGetRelease(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Get Release").
		Assess("Install and Get nginx release", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithNamespace(namespace),
			)
			assert.Nil(t, err)
			defer cli.Close()

			chartRef := "oci://registry-1.docker.io/bitnamicharts/nginx"
			releaseName := "test-get-release"

			// Install a release
			t.Log("Installing release")
			rel, err := cli.Install(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						TakeOwnership: true,
					},
				},
			)
			assert.Nil(t, err)

			// Get the release
			t.Log("Getting release")
			gotRel, err := cli.GetRelease(ctx, releaseName, &helmconfig.GetConfig{})
			assert.Nil(t, err)
			assert.NotNil(t, gotRel)
			assert.Equal(t, releaseName, gotRel.Name)
			assert.Equal(t, rel.Revision, gotRel.Revision)
			assert.Equal(t, "deployed", string(gotRel.Status))

			// Test getting non-existent release
			t.Log("Getting non-existent release")
			notFound, err := cli.GetRelease(ctx, "does-not-exist", &helmconfig.GetConfig{})
			assert.Nil(t, err)
			assert.Nil(t, notFound)

			// Cleanup
			err = cli.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{})
			assert.Nil(t, err)

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

func TestListReleases(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("List Releases").
		Assess("Install multiple releases and list them", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithNamespace(namespace),
			)
			assert.Nil(t, err)
			defer cli.Close()

			chartRef := "oci://registry-1.docker.io/bitnamicharts/nginx"

			// Install multiple releases
			releases := []string{"test-list-1", "test-list-2"}
			for _, releaseName := range releases {
				t.Logf("Installing release: %s", releaseName)
				_, err := cli.Install(
					ctx,
					releaseName,
					chartRef,
					&helmconfig.InstallConfig{
						ActionConfig: &helmconfig.ActionConfig{
							TakeOwnership: true,
						},
					},
				)
				assert.Nil(t, err)
			}

			time.Sleep(5 * time.Second) // Wait for releases to be fully registered

			// List all releases
			t.Log("Listing releases")
			list, err := cli.ListReleases(ctx, &helmconfig.ListConfig{
				All:       true, // List all releases regardless of limit/offset
				StateMask: helmconfig.ListDeployed,
			})
			assert.Nil(t, err)
			t.Logf("Found %d releases", len(list))
			for _, rel := range list {
				t.Logf("  - %s (namespace: %s, status: %s)", rel.Name, rel.Namespace, rel.Status)
			}
			assert.GreaterOrEqual(t, len(list), 2)

			// Verify our releases are in the list
			releaseNames := make(map[string]bool)
			for _, rel := range list {
				releaseNames[rel.Name] = true
			}
			for _, name := range releases {
				assert.True(t, releaseNames[name], "Release %s should be in the list", name)
			}

			// Cleanup
			for _, releaseName := range releases {
				err = cli.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{})
				assert.Nil(t, err)
			}

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

func TestInstallAndUninstall_Repo(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Install and Uninstall from Repo").
		Assess("Install and Uninstall ingress-nginx from repo", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// 1. Initialize Client
			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithNamespace(namespace),
			)
			assert.Nil(t, err)
			defer cli.Close()

			// 3. Define Chart Details - Using official Kubernetes ingress-nginx repo
			chartRef := "https://kubernetes.github.io/ingress-nginx"
			releaseName := "test-release-repo"
			chartName := "ingress-nginx"

			t.Logf("Attempting to install chart %s from repo: %s", chartName, chartRef)

			// 4. Run Install
			rel, err := cli.Install(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						ChartName:     chartName,
						ChartVersion:  "4.10.0", // Pin version for stability
						Timeout:       2 * time.Minute,
						TakeOwnership: true,
					},
				},
			)
			assert.Nil(t, err)
			assert.Equal(t, releaseName, rel.Name)
			assert.Equal(t, "deployed", string(rel.Status))

			t.Logf("Successfully installed release: %s (v%d)", rel.Name, rel.Revision)

			// 5. Run Uninstall
			t.Logf("Attempting to uninstall release: %s", releaseName)
			err = cli.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{})
			assert.Nil(t, err)

			t.Log("Successfully uninstalled release")

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

func TestInstallAndUninstall_TGZ(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Install and Uninstall TGZ Chart").
		Assess("Install and Uninstall from .tgz archive", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// 1. Initialize Client
			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithNamespace(namespace),
			)
			assert.Nil(t, err)
			defer cli.Close()

			// 3. Define Chart Details - Direct .tgz URL
			// Using official Kubernetes ingress-nginx chart archive
			chartRef := "https://github.com/kubernetes/ingress-nginx/releases/download/helm-chart-4.10.0/ingress-nginx-4.10.0.tgz"
			releaseName := "test-release-tgz"

			t.Logf("Attempting to install chart from tgz: %s", chartRef)

			// 4. Run Install
			rel, err := cli.Install(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						ChartVersion:  "4.10.0",
						Timeout:       2 * time.Minute,
						TakeOwnership: true,
					},
				},
			)
			assert.Nil(t, err)
			assert.Equal(t, releaseName, rel.Name)
			assert.Equal(t, "deployed", string(rel.Status))

			t.Logf("Successfully installed release: %s (v%d)", rel.Name, rel.Revision)

			// 5. Run Uninstall
			t.Logf("Attempting to uninstall release: %s", releaseName)
			err = cli.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{})
			assert.Nil(t, err)

			t.Log("Successfully uninstalled release")

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

// prereqReady is what "ready" MEANS for one prerequisite. Different prerequisites are installed
// for different reasons, so a single definition of ready is wrong for at least one of them:
// snowplow is installed purely for the CRDs it registers and its Deployment can never become
// ready, while cnpg is useless until its admission webhook answers. Making each call site say
// what it is waiting for keeps that visible instead of encoding it in a shared default.
type prereqReady func(t *testing.T, c *envconf.Config, ctx context.Context, release string)

// ensurePrerequisite installs a dependency release unless it is already present, then blocks
// until `ready` says it is usable — on BOTH paths.
//
// Existing is not the same as usable, and the difference is not theoretical: a run reused a
// `deployed` cnpg release whose operator pod was not up, and the subject install died on
// `failed calling webhook "mcluster.cnpg.io": dial tcp ...:443: connect: connection refused`.
// The next run passed because the operator had come up by then — the suite was timing-dependent
// on a condition nobody checked. helm's Wait covers a fresh install and cannot cover reuse,
// because there is no install to wait on; `ready` covers both.
//
// Existence is asked, not inferred: GetRelease in the release's own namespace returns (nil, nil)
// when absent. An earlier version matched helm's error MESSAGE for "already in use", because
// GetConfig carried no namespace and GetRelease could not be pointed at these namespaces. That
// was a workaround for a missing parameter — it would pass silently on a reworded error, and it
// could not tell "already installed" from any other failure mentioning those words.
// GetConfig.Namespace now exists, so the question is asked directly.
func ensurePrerequisite(t *testing.T, c *envconf.Config, cli helmconfig.Client, ctx context.Context, name, namespace, repo string, ready prereqReady, cfg *helmconfig.InstallConfig) {
	t.Helper()

	existing, err := cli.GetRelease(ctx, name, &helmconfig.GetConfig{Namespace: namespace})
	require.NoError(t, err, "checking whether prerequisite release %q already exists in %q", name, namespace)
	if existing != nil {
		t.Logf("prerequisite release %q already present in %q (status %s); reusing it", name, namespace, existing.Status)
	} else {
		_, err = cli.Install(ctx, name, repo, cfg)
		require.NoError(t, err, "installing prerequisite release %q into %q", name, namespace)
	}

	if ready != nil {
		ready(t, c, ctx, name)
	}
}

// deploymentsReady waits until every Deployment belonging to the release has a ready replica.
//
// Selected by helm's standard app.kubernetes.io/instance label. A release that sets no such
// Deployments is treated as ready: the check cannot be stricter than the labels allow, and that
// is why a prerequisite whose readiness genuinely matters should say so with a more specific
// expectation rather than relying on this one finding something.
func deploymentsReady(t *testing.T, c *envconf.Config, ctx context.Context, release string) {
	t.Helper()
	cs, err := kubernetes.NewForConfig(c.Client().RESTConfig())
	require.NoError(t, err, "building clientset to check prerequisite %q", release)

	deadline := time.Now().Add(3 * time.Minute)
	for {
		deps, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/instance=" + release,
		})
		require.NoError(t, err, "listing Deployments of prerequisite %q", release)

		notReady := []string{}
		for i := range deps.Items {
			if deps.Items[i].Status.ReadyReplicas < 1 {
				notReady = append(notReady, deps.Items[i].Namespace+"/"+deps.Items[i].Name)
			}
		}
		if len(notReady) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("prerequisite %q still not ready after 3m: Deployments with no ready replica: %v\n"+
				"the release exists, so this is an operator that never came up — anything depending on "+
				"its admission webhooks will fail with connection refused", release, notReady)
		}
		time.Sleep(2 * time.Second)
	}
}

// kindServed waits until the API server serves a kind, which is what a chart rendering that kind
// actually needs. It tests the exact condition whose absence produces
// `no matches for kind "X" in version "Y": ensure CRDs are installed first`.
//
// Used where a prerequisite is installed FOR ITS CRDs rather than for a workload — in which case
// waiting on pods would be both wrong and, for a workload that never becomes ready, fatal.
func kindServed(groupVersion, kind string) prereqReady {
	return func(t *testing.T, c *envconf.Config, ctx context.Context, release string) {
		t.Helper()
		cs, err := kubernetes.NewForConfig(c.Client().RESTConfig())
		require.NoError(t, err, "building clientset to check %s %s from %q", groupVersion, kind, release)

		deadline := time.Now().Add(3 * time.Minute)
		for {
			list, err := cs.Discovery().ServerResourcesForGroupVersion(groupVersion)
			if err == nil {
				for _, r := range list.APIResources {
					if r.Kind == kind {
						return
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s still not served 3m after prerequisite %q; a chart rendering it will "+
					"fail with \"no matches for kind\"", groupVersion, kind, release)
			}
			time.Sleep(2 * time.Second)
		}
	}
}

func TestFullStackAppRedisUpgrade(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Upgrade Full Stack App").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// 0. Create demo-system namespace
			_ = exec.Command("kubectl", "--kubeconfig", c.KubeconfigFile(), "create", "namespace", "demo-system").Run()

			// 1. Install frontend-chart CRDs
			tmpDir := t.TempDir()
			cmd := exec.Command("git", "clone", "--depth", "1", "https://github.com/krateoplatformops/frontend-chart.git", tmpDir)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "failed to clone frontend-chart: %s", string(out))

			crdsDir := filepath.Join(tmpDir, "crd-chart", "templates")
			cmd = exec.Command("kubectl", "--kubeconfig", c.KubeconfigFile(), "apply", "-f", crdsDir)
			out, err = cmd.CombinedOutput()
			require.NoError(t, err, "failed to apply CRDs: %s", string(out))

			// 2. Initialize Helm Client for Setup
			cli, err := NewClient(c.Client().RESTConfig(), WithCache())
			require.NoError(t, err)
			defer cli.Close()

			// 3. Install Redis Operator
			t.Log("Installing Redis Operator")
			ensurePrerequisite(t, c, cli, ctx, "redis-operator", "ot-operators", "https://ot-container-kit.github.io/helm-charts",
				// its operator must be up to reconcile the Redis CR the subject chart creates
				deploymentsReady,
				&helmconfig.InstallConfig{
					Namespace:       "ot-operators",
					CreateNamespace: true,
					ActionConfig: &helmconfig.ActionConfig{
						ChartName: "redis-operator",
						Wait:      true,
						Timeout:   120 * time.Second,
					},
				})

			// 4. Install CNPG Operator
			t.Log("Installing CNPG Operator")
			ensurePrerequisite(t, c, cli, ctx, "cnpg", "", "https://cloudnative-pg.github.io/charts",
				// its admission webhook must answer, or the subject install fails on mcluster.cnpg.io
				deploymentsReady,
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						ChartName:    "cloudnative-pg",
						ChartVersion: "0.27.1",
						Wait:         true,
						Timeout:      15 * time.Minute,
					},
				})

			// 5. Install PG Cluster
			t.Log("Installing PG Cluster")
			ensurePrerequisite(t, c, cli, ctx, "pg-cluster", "", "https://cloudnative-pg.github.io/charts",
				// a data-plane Cluster CR, not an operator: nothing here connects to the database,
				// and CNPG manages its pods directly so there is no Deployment to wait on
				nil,
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						ChartName:    "cluster",
						ChartVersion: "0.5.0",
						Wait:         true,
						Timeout:      15 * time.Minute,
						Values: map[string]interface{}{
							"fullnameOverride": "pg-cluster",
						},
					},
				})

			// 6. Snowplow — installed for its CRDs, NOT for its workload.
			//
			// The subject chart renders RESTAction objects (templates.krateo.io/v1), which only exist
			// because this chart registers them. Its Deployment, by contrast, can never become ready:
			// it mounts a `jwt-sign-key` secret the suite does not create, so the pod sits in
			// CreateContainerConfigError indefinitely — measured at 15h old, 0 restarts, never ready,
			// while the test passed throughout.
			//
			// So its readiness expectation is the CRD being served, not a pod being up. An earlier
			// version of this change removed the prerequisite entirely on the evidence that nothing
			// asserts on snowplow and its pod never runs. That was wrong, and only a FRESH cluster
			// showed it: a reused one still had the CRDs from a previous run, so the render kept
			// working. "The workload is unused" is not "the release is unused".
			t.Log("Installing Snowplow (for templates.krateo.io CRDs)")
			ensurePrerequisite(t, c, cli, ctx, "snowplow", "", "https://charts.krateo.io",
				kindServed("templates.krateo.io/v1", "RESTAction"),
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						ChartName:    "snowplow",
						ChartVersion: "0.20.6",
						// Deliberately NOT Wait: helm would block until the Deployment is ready, and it
						// never is (see above). The CRD gate below is what this install is waited on.
					},
				})

			return ctx
		}).
		Assess("Install with Redis enabled, then upgrade with Redis disabled", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cli, err := NewClient(c.Client().RESTConfig(),
				WithCache(),
				WithNamespace(namespace),
			)
			require.NoError(t, err)
			defer cli.Close()

			chartRef := "https://marketplace.krateo.io"
			chartName := "full-stack-app"
			chartVersion := "0.0.15"
			releaseName := "test-redis-upgrade"

			// 1. Install with Redis enabled
			t.Log("Installing with Redis enabled")
			_, err = cli.Install(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.InstallConfig{
					ActionConfig: &helmconfig.ActionConfig{
						ChartName:     chartName,
						ChartVersion:  chartVersion,
						TakeOwnership: true,
						Verbose:       true,
						Values: map[string]interface{}{
							"app": map[string]interface{}{
								"backend": map[string]interface{}{
									"healthPath": "/api/health",
									"image": map[string]interface{}{
										"repository": "ghcr.io/krateoplatformops-blueprints/pixel-grid-backend",
										"tag":        "latest",
									},
									"port": 8080,
									"service": map[string]interface{}{
										"port": 30098,
										// ClusterIP: see the snowplow service above — a pinned
										// NodePort is a cluster-scoped allocation and makes the
										// suite runnable once per cluster.
										"type": "ClusterIP",
									},
								},
								"cnpg": map[string]interface{}{
									"clusterName":     "pg-cluster",
									"instances":       3,
									"postgresVersion": "18",
									"storage": map[string]interface{}{
										"size": "1Gi",
									},
								},
								"frontend": map[string]interface{}{
									"image": map[string]interface{}{
										"repository": "ghcr.io/krateoplatformops-blueprints/pixel-grid-frontend",
										"tag":        "latest",
									},
									"port": 8080,
									"service": map[string]interface{}{
										"port": 30099,
										// ClusterIP: see the snowplow service above — a pinned
										// NodePort is a cluster-scoped allocation and makes the
										// suite runnable once per cluster.
										"type": "ClusterIP",
									},
								},
								"redis": map[string]interface{}{
									"enabled": true,
									"image":   "quay.io/opstree/redis:v7.4.8",
									"storage": "1Gi",
								},
							},
							"global": map[string]interface{}{
								"compositionApiVersion":       "composition.krateo.io/v0-0-15",
								"compositionGroup":            "composition.krateo.io",
								"compositionId":               "7c9b5120-9996-4911-a03b-7cca5087dd48",
								"compositionInstalledVersion": "v0-0-15",
								"compositionKind":             "FullStackApp",
								"compositionName":             "fsa-1",
								"compositionNamespace":        "demo-system",
								"compositionResource":         "fullstackapps",
								"gracefullyPaused":            "false",
								"krateoNamespace":             "krateo-system",
							},
							"testing": map[string]interface{}{
								"loadTesting": map[string]interface{}{
									"enabled":     false,
									"schedule":    "*/5 * * * *",
									"scriptImage": "ghcr.io/krateoplatformops-blueprints/pixel-grid-load-test:latest",
								},
							},
						},
						PostRenderer: &MyPostRenderer{},
					},
				},
			)
			require.NoError(t, err)

			// Verify that Redis Operator is installed
			redisOperatorObj := &unstructured.Unstructured{}
			redisOperatorObj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "redis.redis.opstreelabs.in",
				Version: "v1beta2",
				Kind:    "Redis",
			})
			err = c.Client().Resources().Get(ctx, releaseName+"-redis", namespace, redisOperatorObj)
			require.NoError(t, err, "Redis CR should exist after install")

			// defer func() {
			// 	_ = cli.Uninstall(ctx, releaseName, &helmconfig.UninstallConfig{IgnoreNotFound: true})
			// }()

			// Optional: verify it is there first
			redisObj := &unstructured.Unstructured{}
			redisObj.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "redis.redis.opstreelabs.in",
				Version: "v1beta2",
				Kind:    "Redis",
			})
			err = c.Client().Resources().Get(ctx, releaseName+"-redis", namespace, redisObj)
			require.NoError(t, err, "Redis CR should exist after install")

			// 			type PostRenderer interface {
			// 	// Run expects the rendered manifests and returns modified manifests
			// 	Run(renderedManifests *bytes.Buffer) (*bytes.Buffer, error)
			// }
			// 2. Upgrade with Redis disabled
			t.Log("Upgrading with Redis disabled")

			logFile, err := os.Create("helm-upgrade.log")
			require.NoError(t, err)
			defer logFile.Close()

			_, err = cli.Upgrade(
				ctx,
				releaseName,
				chartRef,
				&helmconfig.UpgradeConfig{
					ActionConfig: &helmconfig.ActionConfig{
						ChartName:     chartName,
						ChartVersion:  chartVersion,
						TakeOwnership: true,
						Verbose:       true,
						LogWriter:     logFile,
						// NOT a dry run. This upgrade must actually apply, because the assertion
						// below is about what the upgrade DELETES — a component dropped from the
						// render when its feature is disabled. With DryRunServer (as this carried
						// until #52) nothing was applied, so nothing was deleted and the assertion
						// could not pass against any version of this library.
						//
						// That made this the only test anyone had written for disable-on-upgrade,
						// and it proved nothing. The behaviour matters most to the installer, which
						// can disable five components outright and drop others by unpinning
						// (installer#523).
						Values: map[string]interface{}{
							"app": map[string]interface{}{
								"backend": map[string]interface{}{
									"healthPath": "/api/health",
									"image": map[string]interface{}{
										"repository": "ghcr.io/krateoplatformops-blueprints/pixel-grid-backend",
										"tag":        "latest",
									},
									"port": 8080,
									"service": map[string]interface{}{
										"port": 30098,
										// ClusterIP: see the snowplow service above — a pinned
										// NodePort is a cluster-scoped allocation and makes the
										// suite runnable once per cluster.
										"type": "ClusterIP",
									},
								},
								"cnpg": map[string]interface{}{
									"clusterName":     "pg-cluster",
									"instances":       3,
									"postgresVersion": "18",
									"storage": map[string]interface{}{
										"size": "1Gi",
									},
								},
								"frontend": map[string]interface{}{
									"image": map[string]interface{}{
										"repository": "ghcr.io/krateoplatformops-blueprints/pixel-grid-frontend",
										"tag":        "latest",
									},
									"port": 8080,
									"service": map[string]interface{}{
										"port": 30099,
										// ClusterIP: see the snowplow service above — a pinned
										// NodePort is a cluster-scoped allocation and makes the
										// suite runnable once per cluster.
										"type": "ClusterIP",
									},
								},
								"redis": map[string]interface{}{
									"enabled": false,
									"image":   "quay.io/opstree/redis:v7.4.8",
									"storage": "1Gi",
								},
							},
							"global": map[string]interface{}{
								"compositionApiVersion":       "composition.krateo.io/v0-0-15",
								"compositionGroup":            "composition.krateo.io",
								"compositionId":               "7c9b5120-9996-4911-a03b-7cca5087dd48",
								"compositionInstalledVersion": "v0-0-15",
								"compositionKind":             "FullStackApp",
								"compositionName":             "fsa-1",
								"compositionNamespace":        "demo-system",
								"compositionResource":         "fullstackapps",
								"gracefullyPaused":            "false",
								"krateoNamespace":             "krateo-system",
							},
							"testing": map[string]interface{}{
								"loadTesting": map[string]interface{}{
									"enabled":     false,
									"schedule":    "*/5 * * * *",
									"scriptImage": "ghcr.io/krateoplatformops-blueprints/pixel-grid-load-test:latest",
								},
							},
						},
						PostRenderer: &MyPostRenderer{},
					},
				},
			)
			require.NoError(t, err)

			// 3. Verify the Redis CR is actually gone.
			//
			// POLLED, not read once. The Redis CR is operator-managed, so helm issuing the delete
			// is not the end of it: redis-operator has to clear its finalizer before the object
			// leaves the API. A single Get immediately after Upgrade races that and would be flaky
			// in the direction that looks like a product bug. Upgrade is also not configured to
			// wait, and waiting would not help — Wait covers resources helm is creating, not
			// third-party finalization of one it removed.
			//
			// Fails with the object's deletionTimestamp when it has one, because "still
			// terminating after 2m" and "never deleted at all" are different findings and the
			// error should say which.
			deadline := time.Now().Add(2 * time.Minute)
			var lastErr error
			for {
				lastErr = c.Client().Resources().Get(ctx, releaseName+"-redis", namespace, redisObj)
				if kerrors.IsNotFound(lastErr) {
					break
				}
				if time.Now().After(deadline) {
					ts := redisObj.GetDeletionTimestamp()
					if lastErr == nil && ts != nil {
						t.Fatalf("Redis CR %s/%s still present 2m after the upgrade that disabled it; deletionTimestamp=%s, finalizers=%v — helm issued the delete but finalization did not complete",
							namespace, releaseName+"-redis", ts, redisObj.GetFinalizers())
					}
					if lastErr == nil {
						t.Fatalf("Redis CR %s/%s is still present 2m after the upgrade that disabled it, with NO deletionTimestamp — the upgrade never deleted it",
							namespace, releaseName+"-redis")
					}
					t.Fatalf("unexpected error reading Redis CR %s/%s: %v", namespace, releaseName+"-redis", lastErr)
				}
				time.Sleep(2 * time.Second)
			}

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

type MyPostRenderer struct {
}

func (r *MyPostRenderer) Run(renderedManifests *bytes.Buffer) (*bytes.Buffer, error) {
	// Write three sample labels
	nodes, err := kio.FromBytes(renderedManifests.Bytes())
	if err != nil {
		return renderedManifests, fmt.Errorf("failed to parse rendered manifests: %w", err)
	}
	for _, v := range nodes {
		labels := v.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		// your labels
		labels["krateo.io/composition-id"] = "7c9b5120-9996-4911-a03b-7cca5087dd48"
		labels["krateo.io/composition-group"] = "composition.krateo.io"
		labels["krateo.io/composition-installed-version"] = "v0-0-15"
		labels["krateo.io/composition-resource"] = "fullstackapps"
		labels["krateo.io/composition-name"] = "fsa-1"
		labels["krateo.io/composition-namespace"] = "demo-system"
		labels["krateo.io/composition-kind"] = "FullStackApp"
		labels["krateo.io/krateo-namespace"] = "krateo-system"
		v.SetLabels(labels)
	}

	str, err := kio.StringAll(nodes)
	if err != nil {
		return renderedManifests, fmt.Errorf("failed to convert nodes to string: %w", err)
	}

	return bytes.NewBufferString(str), nil
}
