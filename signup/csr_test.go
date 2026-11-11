package signup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	certv1 "k8s.io/api/certificates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

var csrGVR = schema.GroupVersionResource{Group: "certificates.k8s.io", Version: "v1", Resource: "certificatesigningrequests"}

// signer makes the fake apiserver issue a certificate for every CSR it holds,
// as kube-controller-manager's signer does once one is approved.
func signer(c *fake.Clientset) {
	c.PrependReactor("get", "certificatesigningrequests", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := c.Tracker().Get(csrGVR, "", a.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		csr := obj.(*certv1.CertificateSigningRequest).DeepCopy()
		csr.Status.Certificate = []byte("CERT")
		return true, csr, nil
	})
}

func csrNames(t *testing.T, c *fake.Clientset) []string {
	t.Helper()
	objs, err := c.Tracker().List(csrGVR, schema.GroupVersionKind{Group: "certificates.k8s.io", Version: "v1", Kind: "CertificateSigningRequest"}, "")
	require.NoError(t, err)
	var names []string
	for _, o := range objs.(*certv1.CertificateSigningRequestList).Items {
		names = append(names, o.Name)
	}
	return names
}

var opts = generateClientCertAndKeyOpts{userID: "id", username: "authn", groups: []string{"authn"}}

// The CSR is single-use: none is left once the certificate is read, nor when
// a step after its create fails. An approved CSR left under its fixed name is
// an attack surface (authn#119 review).
func TestTheCSRIsDeletedOnEveryWayOut(t *testing.T) {
	t.Run("issued", func(t *testing.T) {
		c := fake.NewClientset()
		signer(c)
		crt, key, err := generateClientCertAndKey(t.Context(), c, opts)
		require.NoError(t, err)
		assert.NotEmpty(t, crt)
		assert.NotEmpty(t, key)
		assert.Empty(t, csrNames(t, c), "no CSR is left behind")
	})

	t.Run("approval refused", func(t *testing.T) {
		c := fake.NewClientset()
		signer(c)
		c.PrependReactor("update", "certificatesigningrequests", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() == "approval" {
				return true, nil, apierrors.NewForbidden(csrGVR.GroupResource(), "authn", errors.New("no approve"))
			}
			return false, nil, nil
		})
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.ErrorContains(t, err, "approving", "the original error is kept")
		assert.Empty(t, csrNames(t, c), "the refused CSR is deleted too")
	})

	t.Run("a stale CSR under the name is replaced, and then deleted", func(t *testing.T) {
		stale := &certv1.CertificateSigningRequest{}
		stale.Name = opts.username
		c := fake.NewClientset(stale)
		signer(c)
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.NoError(t, err)
		assert.Empty(t, csrNames(t, c))
	})

	// failingGet fails the nth get of a CSR (counting from 1) and serves every
	// other from the tracker with the certificate issued, as signer does, so
	// the cleanup delete's own wait still sees the CSR go.
	failingGet := func(c *fake.Clientset, nth int) {
		gets := 0
		c.PrependReactor("get", "certificatesigningrequests", func(a k8stesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets == nth {
				return true, nil, apierrors.NewServiceUnavailable("apiserver restarting")
			}
			obj, err := c.Tracker().Get(csrGVR, "", a.(k8stesting.GetAction).GetName())
			if err != nil {
				return true, nil, err
			}
			csr := obj.(*certv1.CertificateSigningRequest).DeepCopy()
			csr.Status.Certificate = []byte("CERT")
			return true, csr, nil
		})
	}

	t.Run("the wait for the certificate fails", func(t *testing.T) {
		c := fake.NewClientset()
		failingGet(c, 1) // the wait's first poll
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.ErrorContains(t, err, "waiting for CSR certificate")
		assert.Empty(t, csrNames(t, c), "the CSR is deleted")
	})

	t.Run("reading the certificate fails", func(t *testing.T) {
		c := fake.NewClientset()
		failingGet(c, 2) // the wait saw the certificate; the read after it fails
		crt, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.ErrorContains(t, err, "getting CSR")
		assert.Empty(t, crt)
		assert.Empty(t, csrNames(t, c), "the CSR is deleted")
	})

	t.Run("a step and the delete both fail: both are reported, the step first", func(t *testing.T) {
		c := fake.NewClientset()
		signer(c)
		c.PrependReactor("update", "certificatesigningrequests", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if a.GetSubresource() == "approval" {
				return true, nil, apierrors.NewForbidden(csrGVR.GroupResource(), "authn", errors.New("no approve"))
			}
			return false, nil, nil
		})
		c.PrependReactor("delete", "certificatesigningrequests", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(csrGVR.GroupResource(), "authn", errors.New("no delete"))
		})
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.Error(t, err)
		msg := err.Error()
		assert.Contains(t, msg, "approving")
		assert.Contains(t, msg, "deleting the used CSR")
		assert.Less(t, strings.Index(msg, "approving"), strings.Index(msg, "deleting the used CSR"), "the step's error comes first")
	})

	t.Run("a delete that fails after issuance fails the signup", func(t *testing.T) {
		c := fake.NewClientset()
		signer(c)
		c.PrependReactor("delete", "certificatesigningrequests", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(csrGVR.GroupResource(), "authn", errors.New("no delete"))
		})
		crt, key, err := generateClientCertAndKey(t.Context(), c, opts)
		require.ErrorContains(t, err, "deleting the used CSR")
		assert.Empty(t, crt, "nothing is returned for the caller to store")
		assert.Empty(t, key)
	})
}

// The cleanup delete is bounded (plumbing#129): an apiserver that accepts the
// delete but never confirms it holds the signup csrCleanupTimeout at most, not
// the 5 minutes of certificateWaitTimeout, and the CSR left behind is reported.
func TestTheCleanupDeleteIsBounded(t *testing.T) {
	old := csrCleanupTimeout
	csrCleanupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { csrCleanupTimeout = old })

	c := fake.NewClientset()
	signer(c)
	// Accepted, never done: the tracker keeps serving the CSR.
	c.PrependReactor("delete", "certificatesigningrequests", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	start := time.Now()
	crt, _, err := generateClientCertAndKey(t.Context(), c, opts)
	elapsed := time.Since(start)
	require.ErrorContains(t, err, "deleting the used CSR")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, crt, "nothing is returned for the caller to store")
	// The certificate wait's first poll (certs' certificateWaitPollInternval,
	// 3s) comes before the cleanup; after it, only csrCleanupTimeout, not the 5
	// minutes of certificateWaitTimeout.
	const certificatePoll = 3 * time.Second
	assert.Less(t, elapsed, certificatePoll+2*time.Second, "bounded by csrCleanupTimeout")
}

// A caller whose context is cancelled mid-signup still has the CSR it created
// cleaned up: the cleanup runs without the caller's cancellation. The client
// is client-go over HTTP, which honors the context (the fake clientset
// ignores it, so it can't show this).
func TestACancelledSignupStillDeletesItsCSR(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	api := newCSRServer(t, func() { cancel() }) // the caller goes away at the approval
	client, err := kubernetes.NewForConfig(&rest.Config{Host: api.srv.URL,
		ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}})
	require.NoError(t, err)

	_, _, err = generateClientCertAndKey(ctx, client, opts)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, api.names(), "the CSR is deleted though the caller cancelled")
	assert.Positive(t, api.deletes.Load(), "the cleanup reached the apiserver")
}

// csrServer is an in-memory apiserver for CSRs: create, approval, get and
// delete. It never issues a certificate. onApproval runs when an approval
// arrives.
type csrServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	csrs    map[string][]byte
	deletes atomic.Int32
}

func newCSRServer(t *testing.T, onApproval func()) *csrServer {
	t.Helper()
	const base = "/apis/certificates.k8s.io/v1/certificatesigningrequests"
	s := &csrServer{csrs: map[string][]byte{}}
	notFound := func(w http.ResponseWriter, name string) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"%s not found"}`, name)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")
		name, sub, _ := strings.Cut(rest, "/")
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && name == "":
			var csr certv1.CertificateSigningRequest
			require.NoError(t, json.Unmarshal(body, &csr))
			s.csrs[csr.Name] = body
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(body)
		case r.Method == http.MethodPut && sub == "approval":
			if onApproval != nil {
				onApproval()
			}
			_, _ = w.Write(body)
		case r.Method == http.MethodGet && name != "":
			b, ok := s.csrs[name]
			if !ok {
				notFound(w, name)
				return
			}
			_, _ = w.Write(b)
		case r.Method == http.MethodDelete && name != "":
			s.deletes.Add(1)
			if _, ok := s.csrs[name]; !ok {
				notFound(w, name)
				return
			}
			delete(s.csrs, name)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
		default:
			t.Errorf("unexpected apiserver call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *csrServer) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n := range s.csrs {
		out = append(out, n)
	}
	return out
}

// A create answered with an error after the apiserver stored the CSR (a
// timeout after the write) leaves none behind (plumbing#129); one that stored
// nothing reports the create's error alone.
func TestAFailedCreateLeavesNoCSR(t *testing.T) {
	t.Run("stored, then answered 504", func(t *testing.T) {
		c := fake.NewClientset()
		c.PrependReactor("create", "certificatesigningrequests", func(a k8stesting.Action) (bool, runtime.Object, error) {
			obj := a.(k8stesting.CreateAction).GetObject()
			require.NoError(t, c.Tracker().Create(csrGVR, obj, ""))
			return true, nil, apierrors.NewTimeoutError("the write may have succeeded", 0)
		})
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.ErrorContains(t, err, "creating CSR kubernetes object")
		assert.NotContains(t, err.Error(), "whose create failed", "the delete succeeded")
		assert.Empty(t, csrNames(t, c), "the stored CSR is deleted")
	})

	t.Run("nothing stored", func(t *testing.T) {
		c := fake.NewClientset()
		c.PrependReactor("create", "certificatesigningrequests", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(csrGVR.GroupResource(), "authn", errors.New("no create"))
		})
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.ErrorContains(t, err, "creating CSR kubernetes object")
		assert.NotContains(t, err.Error(), "whose create failed", "NotFound means nothing was stored")
	})

	t.Run("stored, and the delete fails too: both are reported", func(t *testing.T) {
		c := fake.NewClientset()
		c.PrependReactor("create", "certificatesigningrequests", func(a k8stesting.Action) (bool, runtime.Object, error) {
			require.NoError(t, c.Tracker().Create(csrGVR, a.(k8stesting.CreateAction).GetObject(), ""))
			return true, nil, apierrors.NewTimeoutError("the write may have succeeded", 0)
		})
		c.PrependReactor("delete", "certificatesigningrequests", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(csrGVR.GroupResource(), "authn", errors.New("no delete"))
		})
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.Error(t, err)
		msg := err.Error()
		assert.Contains(t, msg, "creating CSR kubernetes object")
		assert.Contains(t, msg, "whose create failed")
		assert.Less(t, strings.Index(msg, "creating CSR"), strings.Index(msg, "whose create failed"), "the create's error first")
	})
}
