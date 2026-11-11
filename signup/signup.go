package signup

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/krateo-platformops/plumbing/certs"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/kubeutil"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Options struct {
	CAData       string
	ProxyURL     string
	ServerURL    string
	CertDuration time.Duration
	RestConfig   *rest.Config
	Namespace    string
	Username     string
	UserGroups   []string
}

func Do(ctx context.Context, opts Options) (endpoints.Endpoint, error) {
	ep, _, err := DoWritten(ctx, opts)
	return ep, err
}

// DoWritten is Do, also reporting what it stored: the credential Secret, the
// verb sent and the apiserver's answer (endpoints.StoreWritten). It is empty
// when signup failed before the write.
func DoWritten(ctx context.Context, opts Options) (endpoints.Endpoint, endpoints.Written, error) {
	handler := &signupHandler{
		caData:       opts.CAData,
		proxyURL:     opts.ProxyURL,
		serverURL:    opts.ServerURL,
		certDuration: opts.CertDuration,
		restconfig:   opts.RestConfig,
		namespace:    opts.Namespace,
	}

	return handler.doIt(ctx, opts.Username, opts.UserGroups)
}

type signupHandler struct {
	caData       string
	proxyURL     string
	serverURL    string
	certDuration time.Duration
	restconfig   *rest.Config
	namespace    string
}

func (g *signupHandler) doIt(ctx context.Context, user string, groups []string) (endpoints.Endpoint, endpoints.Written, error) {
	if len(g.caData) == 0 {
		caCrt, err := kubeutil.CACrt(ctx, g.restconfig)
		if err != nil {
			return endpoints.Endpoint{}, endpoints.Written{}, err
		}
		g.caData = caCrt
	}

	ep, err := g.generateEndpoint(user, groups)
	if err != nil {
		return endpoints.Endpoint{}, endpoints.Written{}, err
	}
	ep.Username = user

	w, err := endpoints.StoreWritten(ctx, g.restconfig, g.namespace, ep)
	return ep, w, err
}

func (g *signupHandler) generateEndpoint(user string, groups []string) (ep endpoints.Endpoint, err error) {
	if len(g.serverURL) == 0 {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if len(host) == 0 || len(port) == 0 {
			return ep, rest.ErrNotInCluster
		}
		g.serverURL = "https://" + net.JoinHostPort(host, port)
	}

	cli, err := kubernetes.NewForConfig(g.restconfig)
	if err != nil {
		return ep, err
	}

	cert, key, err := generateClientCertAndKey(cli, generateClientCertAndKeyOpts{
		userID:   mkID(fmt.Sprintf("%s@%s", user, strings.Join(groups, ","))),
		username: user,
		groups:   groups,
		duration: g.certDuration,
	})
	if err != nil {
		return ep, err
	}

	ep.ServerURL = g.serverURL
	ep.CertificateAuthorityData = g.caData
	ep.ClientCertificateData = cert
	ep.ClientKeyData = key

	return
}

type generateClientCertAndKeyOpts struct {
	duration time.Duration
	userID   string
	username string
	groups   []string
}

func generateClientCertAndKey(client kubernetes.Interface, o generateClientCertAndKeyOpts) (string, string, error) {
	key, err := certs.NewPrivateKey()
	if err != nil {
		return "", "", err
	}

	req, err := certs.NewCertificateRequest(certs.CertificateRequestOptions{
		Key: key, Username: o.username, Groups: o.groups,
	})
	if err != nil {
		return "", "", err
	}

	csr := certs.NewCertificateSigningRequest(certs.CertificateSigningRequestOptions{
		CSR: req, Duration: o.duration,
		UserID: o.userID, Username: o.username,
	})

	err = certs.CreateCertificateSigningRequests(client, csr)
	if err != nil {
		if !errors.IsAlreadyExists(err) {
			return "", "", fmt.Errorf("creating CSR kubernetes object: %w", err)
		}

		if err := certs.DeleteCertificateSigningRequest(client, csr.Name); err != nil {
			return "", "", fmt.Errorf("deleting existing CSR kubernetes object: %w", err)
		}

		if err := certs.CreateCertificateSigningRequests(client, csr); err != nil {
			return "", "", fmt.Errorf("creating CSR kubernetes object: %w", err)
		}
	}

	err = certs.ApproveCertificateSigningRequest(client, csr, "authn")
	if err != nil {
		return "", "", err
	}

	err = certs.WaitForCertificate(client, csr.Name)
	if err != nil {
		return "", "", err
	}

	crt, err := certs.Certificate(client, csr.Name)
	if err != nil {
		return "", "", err
	}

	crtStr := base64.StdEncoding.EncodeToString(crt)
	keyStr := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	return crtStr, keyStr, nil
}

func mkID(in string) string {
	hash := fnv.New64a()
	hash.Write([]byte(in))
	return strconv.FormatUint(hash.Sum64(), 16)
}
