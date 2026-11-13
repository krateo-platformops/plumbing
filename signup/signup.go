package signup

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	stderrors "errors"
	"github.com/krateo-platformops/plumbing/certs"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/kubeutil"
	certv1 "k8s.io/api/certificates/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
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

	ep, err := g.generateEndpoint(ctx, user, groups)
	if err != nil {
		return endpoints.Endpoint{}, endpoints.Written{}, err
	}
	ep.Username = user

	w, err := endpoints.StoreWritten(ctx, g.restconfig, g.namespace, ep)
	return ep, w, err
}

func (g *signupHandler) generateEndpoint(ctx context.Context, user string, groups []string) (ep endpoints.Endpoint, err error) {
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

	cert, key, err := generateClientCertAndKey(ctx, cli, generateClientCertAndKeyOpts{
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

// csrCleanupTimeout bounds each delete of the CSR signup makes on the way out
// (plumbing#129): an apiserver that accepts a delete but never confirms it,
// or doesn't answer, holds the signup this long at most, not
// certificateWaitTimeout.
var csrCleanupTimeout = 30 * time.Second

// cleanupContext is the context a cleanup delete runs on: ctx's values without
// its cancellation, so a cancelled request still cleans up the CSR it created,
// bounded by csrCleanupTimeout.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), csrCleanupTimeout)
}

// csrSuffixLen is the length of the random hex suffix that makes each
// signup's CSR name its own: 40 bits.
const csrSuffixLen = 10

// newCSRSuffix returns csrSuffixLen random hex digits. A variable so a test
// can force a collision.
var newCSRSuffix = func() (string, error) {
	b := make([]byte, csrSuffixLen/2)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating the CSR name: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// csrName names one signup's CSR: username, a dash and csrSuffixLen random
// hex digits, so two signups for the same user never share a name
// (plumbing#137). The name is chosen before the create rather than by the
// apiserver (generateName), so a create whose answer is lost after the
// apiserver stored it can still be cleaned up by name (plumbing#129).
// username is cut so the name stays a DNS-1123 subdomain of at most 253
// characters, and a cut never leaves a trailing '.' or '-' before the dash.
func csrName(username string) (string, error) {
	suffix, err := newCSRSuffix()
	if err != nil {
		return "", err
	}
	if max := validation.DNS1123SubdomainMaxLength - 1 - csrSuffixLen; len(username) > max {
		username = strings.TrimRight(username[:max], ".-")
	}
	return username + "-" + suffix, nil
}

// deleteOwnCSR is the cleanup delete of the CSR named after csr, and only if
// it is provably this signup's (plumbing#134 review). The name is this
// signup's own (csrName), but the checks stay: a CSR whose spec.request isn't
// csr's (another key) is left alone, and the one delete carries a UID
// precondition, so a CSR re-created between the read and the delete isn't
// touched either. NotFound, or a precondition that no longer holds, means
// there is nothing of ours to delete. It runs on cleanupContext.
func deleteOwnCSR(ctx context.Context, client kubernetes.Interface, csr *certv1.CertificateSigningRequest) error {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	live, err := certs.GetCertificateSigningRequest(cctx, client, csr.Name)
	switch {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !bytes.Equal(live.Spec.Request, csr.Spec.Request):
		return nil // another signup's
	}
	err = certs.DeleteCertificateSigningRequestUID(cctx, client, csr.Name, live.UID)
	if err == nil || errors.IsNotFound(err) || errors.IsConflict(err) {
		return nil
	}
	return err
}

func generateClientCertAndKey(ctx context.Context, client kubernetes.Interface, o generateClientCertAndKeyOpts) (crtOut, keyOut string, err error) {
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
	// The CSR's name is this signup's own, never just the user's: another
	// signup for the same user, in flight or stale, holds another name, so
	// nothing here ever deletes or replaces it (plumbing#137). A create answered
	// AlreadyExists (a 40-bit collision) is an error like any other, and the
	// existing CSR, whose request isn't ours, is left alone.
	if csr.Name, err = csrName(o.username); err != nil {
		return "", "", err
	}

	if err = certs.CreateCertificateSigningRequests(ctx, client, csr); err != nil {
		return "", "", createFailed(ctx, client, csr, err)
	}

	// The CSR is single-use, and an approved one is an attack surface: delete
	// it once the certificate is read, and on every way out after its create,
	// rather than leave it until the apiserver's garbage collection. A CSR an
	// earlier version left under the user's bare name is never this signup's,
	// so it is never touched: the apiserver's CSR garbage collection removes
	// it. On success a failed delete is the result, so
	// the caller retries and the CSR doesn't linger unnoticed; after a failure
	// the original error is kept, joined with the delete's when that fails too.
	defer func() {
		derr := deleteOwnCSR(ctx, client, csr)
		if derr == nil {
			return
		}
		derr = fmt.Errorf("deleting the used CSR %s: %w", csr.Name, derr)
		if err == nil {
			crtOut, keyOut, err = "", "", derr
			return
		}
		// Both failed: the step's error first, and the CSR left behind is not
		// hidden.
		err = stderrors.Join(err, derr)
	}()

	err = certs.ApproveCertificateSigningRequest(ctx, client, csr, "authn")
	if err != nil {
		return "", "", err
	}

	err = certs.WaitForCertificate(ctx, client, csr.Name)
	if err != nil {
		return "", "", err
	}

	crt, err := certs.Certificate(ctx, client, csr.Name)
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

// createFailed is the error of a create that failed. The apiserver may have
// stored this CSR anyway (a timeout after the write), so it is deleted before
// signup returns (plumbing#129), and only if it is ours (deleteOwnCSR): a create
// refused outright, or answered AlreadyExists because another CSR holds the
// name, leaves that CSR alone. A delete that fails too is joined
// after the create's error, so the CSR left behind is not hidden.
func createFailed(ctx context.Context, client kubernetes.Interface, csr *certv1.CertificateSigningRequest, err error) error {
	err = fmt.Errorf("creating CSR kubernetes object: %w", err)
	if derr := deleteOwnCSR(ctx, client, csr); derr != nil {
		return stderrors.Join(err, fmt.Errorf("deleting the CSR %s whose create failed: %w", csr.Name, derr))
	}
	return err
}
