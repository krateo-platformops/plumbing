package endpoints

import (
	"context"
	"fmt"
	"strconv"

	"github.com/krateo-platformops/plumbing/kubeutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

func FromSecret(ctx context.Context, rc *rest.Config, name, namespace string) (Endpoint, error) {
	cli, err := newSecretsRESTClient(rc)
	if err != nil {
		return Endpoint{}, err
	}

	sec, err := getSecret(ctx, clientOptions{
		cli:       cli,
		name:      name,
		namespace: namespace,
	})
	if err != nil {
		return Endpoint{}, err
	}

	res := Endpoint{}
	if v, ok := sec.Data[serverUrlLabel]; ok {
		res.ServerURL = string(v)
	} else {
		return res, fmt.Errorf("missed required attribute for endpoint: server-url")
	}

	if v, ok := sec.Data[proxyUrlLabel]; ok {
		res.ProxyURL = string(v)
	}

	if v, ok := sec.Data[tokenLabel]; ok {
		res.Token = string(v)
	}

	if v, ok := sec.Data[usernameLabel]; ok {
		res.Username = string(v)
	}

	if v, ok := sec.Data[passwordLabel]; ok {
		res.Password = string(v)
	}

	if v, ok := sec.Data[caLabel]; ok {
		res.CertificateAuthorityData = string(v)
	}

	if v, ok := sec.Data[clientKeyLabel]; ok {
		res.ClientKeyData = string(v)
	}

	if v, ok := sec.Data[clientCertLabel]; ok {
		res.ClientCertificateData = string(v)
	}

	if v, ok := sec.Data[debugLabel]; ok {
		res.Debug, _ = strconv.ParseBool(string(v))
	}

	if v, ok := sec.Data[insecureLabel]; ok {
		res.Insecure, _ = strconv.ParseBool(string(v))
	}

	// ParseBool's error is dropped, as for debug and insecure above - but here the failure mode is
	// SAFE: an unparseable value leaves the field false, which REFUSES. A security opt-out that
	// defaulted on when its value was malformed would be the destructive default.
	if v, ok := sec.Data[passCredentialsLabel]; ok {
		res.PassCredentialsOnRedirect, _ = strconv.ParseBool(string(v))
	}

	if v, ok := sec.Data[awsAccessKeyLabel]; ok {
		res.AwsAccessKey = string(v)
	}

	if v, ok := sec.Data[awsSecretKeyLabel]; ok {
		res.AwsSecretKey = string(v)
	}

	if v, ok := sec.Data[awsRegionLabel]; ok {
		res.AwsRegion = string(v)
	}

	if v, ok := sec.Data[awsServiceLabel]; ok {
		res.AwsService = string(v)
	}

	return res, nil
}

// ClientConfigSuffix ends the name of every Secret holding a user's session
// credential.
const ClientConfigSuffix = "-clientconfig"

// SecretName names the Secret holding username's session credential, as authn
// writes it and every consumer reads it: the username made DNS-1123 compatible,
// plus ClientConfigSuffix. It is the one definition of that name; compute it
// here rather than rebuilding it, since a near-miss names a Secret that
// doesn't exist.
func SecretName(username string) string {
	return kubeutil.MakeDNS1123Compatible(username) + ClientConfigSuffix
}

// Written is what Store wrote: the Secret, the verb of the request that stored
// it, and the apiserver's answer to that request. An audit record names the
// object written and the verb sent (AUDIT-4), and neither can be inferred
// afterwards: Store's create is answered 409 when the Secret exists, and the
// update that follows is the write. Names only, never data.
type Written struct {
	Namespace string
	Name      string
	// UID and ResourceVersion are the stored Secret's; empty when nothing was
	// stored.
	UID             string
	ResourceVersion string
	// Verb is "create" or "update": the request that stored the Secret, or the
	// one that failed.
	Verb string
	// StatusCode is the apiserver's HTTP status for that request, 0 when no
	// response arrived.
	StatusCode int
}

// Store writes ep as username's credential Secret in ns: a create, or an update
// when the Secret exists.
func Store(ctx context.Context, rc *rest.Config, ns string, ep Endpoint) error {
	_, err := StoreWritten(ctx, rc, ns, ep)
	return err
}

// StoreWritten is Store, reporting what it wrote. On an error, Written still
// names the Secret and, when a request was sent, its verb and status.
func StoreWritten(ctx context.Context, rc *rest.Config, ns string, ep Endpoint) (Written, error) {
	sec := corev1.Secret{}
	sec.SetName(SecretName(ep.Username))
	sec.SetNamespace(ns)
	sec.StringData = map[string]string{
		usernameLabel:   ep.Username,
		passwordLabel:   ep.Password,
		tokenLabel:      ep.Token,
		caLabel:         ep.CertificateAuthorityData,
		clientCertLabel: ep.ClientCertificateData,
		clientKeyLabel:  ep.ClientKeyData,
		serverUrlLabel:  ep.ServerURL,
		proxyUrlLabel:   ep.ProxyURL,
		debugLabel:      strconv.FormatBool(ep.Debug),
		insecureLabel:   strconv.FormatBool(ep.Insecure),
	}

	w := Written{Namespace: ns, Name: sec.Name}
	cli, err := newSecretsRESTClient(rc)
	if err != nil {
		return w, err
	}
	stored, code, err := createSecret(ctx, cli, &sec)
	if err == nil {
		return written(w, "create", code, stored), nil
	}

	if !apierrors.IsAlreadyExists(err) {
		return written(w, "create", code, nil), err
	}

	stored, code, err = updateSecret(ctx, cli, &sec)
	return written(w, "update", code, stored), err
}

func written(w Written, verb string, code int, stored *corev1.Secret) Written {
	w.Verb, w.StatusCode = verb, code
	if stored != nil {
		w.UID, w.ResourceVersion = string(stored.UID), stored.ResourceVersion
	}
	return w
}

const (
	clientCertLabel      = "client-certificate-data"
	clientKeyLabel       = "client-key-data"
	caLabel              = "certificate-authority-data"
	proxyUrlLabel        = "proxy-url"
	serverUrlLabel       = "server-url"
	debugLabel           = "debug"
	passwordLabel        = "password"
	usernameLabel        = "username"
	tokenLabel           = "token"
	insecureLabel        = "insecure"
	passCredentialsLabel = "pass-credentials-on-redirect"
	awsAccessKeyLabel    = "aws-access-key"
	awsSecretKeyLabel    = "aws-secret-key"
	awsRegionLabel       = "aws-region"
	awsServiceLabel      = "aws-service"
)
