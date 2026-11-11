//go:build integration
// +build integration

package endpoints

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestSecrets(t *testing.T) {
	os.Setenv("DEBUG", "0")

	f := features.New("Setup").
		Assess("Create Secret", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			rc := c.Client().RESTConfig()

			cli, err := newSecretsRESTClient(rc)
			assert.Nil(t, err)

			obj := corev1.Secret{}
			obj.SetName("dummy")
			obj.SetNamespace(namespace)
			obj.StringData = map[string]string{
				"dontTell": "shh!shh!",
			}

			stored, code, err := createSecret(ctx, cli, &obj)
			assert.Nil(t, err)
			assert.Equal(t, 201, code)
			assert.NotEmpty(t, stored.UID, "the stored Secret is returned")

			return ctx
		}).
		Assess("Get Secret", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			rc := c.Client().RESTConfig()

			cli, err := newSecretsRESTClient(rc)
			assert.Nil(t, err)

			want := map[string][]byte{
				"dontTell": []byte("shh!shh!"),
			}

			got, err := getSecret(ctx, clientOptions{cli: cli, name: "dummy", namespace: namespace})
			assert.Nil(t, err)
			assert.Equal(t, want, got.Data)

			return ctx
		}).
		Assess("Update Secret", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			rc := c.Client().RESTConfig()

			cli, err := newSecretsRESTClient(rc)
			assert.Nil(t, err)

			got, err := getSecret(ctx, clientOptions{cli: cli, name: "dummy", namespace: namespace})
			assert.Nil(t, err)

			got.StringData = map[string]string{
				"dontTell": "alreadyDone!",
			}

			stored, code, err := updateSecret(ctx, cli, got)
			assert.Nil(t, err)
			assert.Equal(t, 200, code)
			assert.Equal(t, got.UID, stored.UID)

			want := map[string][]byte{
				"dontTell": []byte("alreadyDone!"),
			}

			got, err = getSecret(ctx, clientOptions{cli: cli, name: "dummy", namespace: namespace})
			assert.Nil(t, err)
			assert.Equal(t, want, got.Data)

			return ctx
		}).
		Assess("Delete Secret", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			rc := c.Client().RESTConfig()

			cli, err := newSecretsRESTClient(rc)
			assert.Nil(t, err)

			err = deleteSecret(ctx, clientOptions{cli: cli, name: "dummy", namespace: namespace})
			assert.Nil(t, err)

			_, err = getSecret(ctx, clientOptions{cli: cli, name: "dummy", namespace: namespace})
			assert.True(t, errors.IsNotFound(err))

			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}
