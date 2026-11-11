package endpoints

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
)

type clientOptions struct {
	cli       *rest.RESTClient
	name      string
	namespace string
}

func newSecretsRESTClient(rc *rest.Config) (*rest.RESTClient, error) {
	gv := schema.GroupVersion{
		Group:   "",
		Version: "v1",
	}

	sb := runtime.NewSchemeBuilder(
		func(reg *runtime.Scheme) error {
			reg.AddKnownTypes(
				gv,
				&corev1.Secret{},
				&corev1.SecretList{},
				&metav1.ListOptions{},
				&metav1.GetOptions{},
				&metav1.DeleteOptions{},
				&metav1.CreateOptions{},
				&metav1.UpdateOptions{},
				&metav1.PatchOptions{},
				&metav1.Status{},
			)
			return nil
		})

	s := runtime.NewScheme()
	sb.AddToScheme(s)

	config := *rc
	config.APIPath = "/api"
	config.GroupVersion = &gv
	config.NegotiatedSerializer = serializer.NewCodecFactory(s).
		WithoutConversion()
	config.UserAgent = rest.DefaultKubernetesUserAgent()

	cli, err := rest.RESTClientFor(&config)
	if err != nil {
		return nil, err
	}

	//pc := runtime.NewParameterCodec(s)

	return cli, nil
}

func getSecret(ctx context.Context, opts clientOptions) (result *corev1.Secret, err error) {
	result = &corev1.Secret{}
	err = opts.cli.Get().
		Namespace(opts.namespace).
		Resource("secrets").
		Name(opts.name).
		Do(ctx).
		Into(result)
	return
}

// createSecret POSTs in and returns the apiserver's status code (0 when no
// response arrived) and, on success, the stored Secret.
func createSecret(ctx context.Context, cli *rest.RESTClient, in *corev1.Secret) (*corev1.Secret, int, error) {
	return storedSecret(cli.Post().
		Namespace(in.GetNamespace()).
		Resource("secrets").
		Body(in).
		Do(ctx))
}

// updateSecret PUTs in, as createSecret.
func updateSecret(ctx context.Context, cli *rest.RESTClient, in *corev1.Secret) (*corev1.Secret, int, error) {
	return storedSecret(cli.Put().
		Namespace(in.GetNamespace()).
		Resource("secrets").
		Name(in.Name).
		Body(in).
		Do(ctx))
}

// storedSecret reads res. The write succeeded when res carries no error, and
// then the error is nil whatever happens next: a response body that can't be
// decoded only leaves the stored Secret unknown (nil), so Store keeps
// succeeding whenever the write did, and an audit record never calls a done
// write failed.
func storedSecret(res rest.Result) (*corev1.Secret, int, error) {
	var code int
	res.StatusCode(&code)
	if err := res.Error(); err != nil {
		return nil, code, err
	}
	out := &corev1.Secret{}
	if err := res.Into(out); err != nil {
		return nil, code, nil
	}
	return out, code, nil
}

func deleteSecret(ctx context.Context, opts clientOptions) error {
	return opts.cli.Delete().
		Namespace(opts.namespace).
		Resource("secrets").
		Name(opts.name).
		Do(ctx).
		Error()
}
