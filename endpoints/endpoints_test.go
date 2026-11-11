//go:build integration
// +build integration

package endpoints

import (
	"context"
	"fmt"
	"github.com/krateo-platformops/plumbing/e2e/kubepin"
	"os"
	"testing"

	xenv "github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/kubeutil"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"
)

var (
	testenv     env.Environment
	clusterName string
	namespace   string
)

const (
	testdataPath = "../../testdata"
)

func TestMain(m *testing.M) {
	// First, before any cluster exists: kind must never touch the developer's
	// kubeconfig, and this suite destroys its cluster at the end.
	restore := kubepin.Pin("endpoints")
	xenv.SetTestMode(true)

	namespace = "demo-system"
	// A name only this suite uses: it destroys the cluster at the end, so a
	// common name (it was "krateo") deletes whatever cluster a developer has
	// by that name.
	clusterName = "plumbing-endpoints-test"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		envfuncs.CreateNamespace(namespace),
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.DestroyCluster(clusterName),
	)

	code := testenv.Run(m)
	restore() // not deferred: os.Exit does not run defers
	os.Exit(code)
}

func TestEndpoints(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Setup").
		Assess("Store and Retrieve", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			want := Endpoint{
				Username:                 "pinoc.pallo@email.com",
				ServerURL:                "https://example.org",
				CertificateAuthorityData: "CA_BLAH_BLAH_BLAH",
				ClientKeyData:            "LORE_IPSUM",
				ClientCertificateData:    "CA_LOREM_IPSUM",
				Token:                    "DONT_TELL!",
			}

			rc := c.Client().RESTConfig()

			err := Store(ctx, rc, namespace, want)
			assert.Nil(t, err)

			name := fmt.Sprintf("%s-clientconfig", kubeutil.MakeDNS1123Compatible(want.Username))
			got, err := FromSecret(ctx, rc, name, namespace)
			assert.Nil(t, err)
			assert.Equal(t, want, got)

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}
