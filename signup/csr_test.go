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

	"github.com/krateo-platformops/plumbing/certs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	certv1 "k8s.io/api/certificates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

// A caller whose context is cancelled mid-signup still has the CSR it created
// cleaned up: the cleanup runs without the caller's cancellation. The client
// is client-go over HTTP, which honors the context (the fake clientset
// ignores it, so it can't show this).
func TestACancelledSignupStillDeletesItsCSR(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after time.Duration // after the approval arrives; 0: at it
	}{
		{"cancelled at the approval", 0},
		// The approval has returned: the cancel lands in the certificate wait,
		// which must stop there, not after the 5 minutes of
		// certificateWaitTimeout (P10).
		{"cancelled during the certificate wait", 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			api := newCSRServer(t, func() { // the caller goes away
				if tc.after == 0 {
					cancel()
					return
				}
				time.AfterFunc(tc.after, cancel)
			})
			start := time.Now()
			_, _, err := generateClientCertAndKey(ctx, jsonClient(t, api.srv), opts)
			require.Error(t, err)
			assert.ErrorIs(t, err, context.Canceled)
			assert.Less(t, time.Since(start), 2*time.Second, "the signup stops at the cancel")
			assert.Empty(t, api.names(), "the CSR is deleted though the caller cancelled")
			assert.Positive(t, api.deletes.Load(), "the cleanup reached the apiserver")
		})
	}
}

// jsonClient is client-go over HTTP to srv, which honors the context and the
// delete's preconditions (the fake clientset honors neither).
func jsonClient(t *testing.T, srv *httptest.Server) kubernetes.Interface {
	t.Helper()
	c, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL,
		ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}})
	require.NoError(t, err)
	return c
}

// others is another signup's CSR for the same user: same name, its own key.
func others(t *testing.T) *certv1.CertificateSigningRequest {
	t.Helper()
	key, err := certs.NewPrivateKey()
	require.NoError(t, err)
	req, err := certs.NewCertificateRequest(certs.CertificateRequestOptions{Key: key, Username: opts.username, Groups: opts.groups})
	require.NoError(t, err)
	return certs.NewCertificateSigningRequest(certs.CertificateSigningRequestOptions{CSR: req, UserID: "other", Username: opts.username})
}

// The cleanup deletes only a CSR that is provably this signup's (plumbing#134
// review): the name is the user's, so another signup for the same user may hold
// it. One whose spec.request is another key's is never deleted, nor one
// re-created between the cleanup's read and its delete (the UID precondition).
func TestTheCleanupNeverDeletesAnotherSignupsCSR(t *testing.T) {
	t.Run("a create refused outright leaves the other signup's CSR", func(t *testing.T) {
		other := others(t)
		c := fake.NewClientset(other)
		c.PrependReactor("create", "certificatesigningrequests", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(csrGVR.GroupResource(), "authn", errors.New("no create"))
		})
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.ErrorContains(t, err, "creating CSR kubernetes object")
		assert.Equal(t, []string{other.Name}, csrNames(t, c), "the other signup's CSR is kept")
	})

	t.Run("a re-create that loses to another signup leaves its CSR", func(t *testing.T) {
		stale := &certv1.CertificateSigningRequest{}
		stale.Name = opts.username
		other := others(t)
		c := fake.NewClientset(stale)
		creates := 0
		c.PrependReactor("create", "certificatesigningrequests", func(a k8stesting.Action) (bool, runtime.Object, error) {
			creates++
			if creates == 2 { // our re-create, after the stale one is gone: the other signup got there first
				require.NoError(t, c.Tracker().Create(csrGVR, other, ""))
				return true, nil, apierrors.NewAlreadyExists(csrGVR.GroupResource(), opts.username)
			}
			return false, nil, nil
		})
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.ErrorContains(t, err, "creating CSR kubernetes object")
		require.Equal(t, []string{other.Name}, csrNames(t, c))
		got, gerr := c.CertificatesV1().CertificateSigningRequests().Get(t.Context(), other.Name, metav1.GetOptions{})
		require.NoError(t, gerr)
		assert.Equal(t, other.Spec.Request, got.Spec.Request, "the other signup's CSR, untouched")
	})

	t.Run("a re-create stored then answered 504 is ours, and is deleted", func(t *testing.T) {
		stale := &certv1.CertificateSigningRequest{}
		stale.Name = opts.username
		c := fake.NewClientset(stale)
		creates := 0
		c.PrependReactor("create", "certificatesigningrequests", func(a k8stesting.Action) (bool, runtime.Object, error) {
			creates++
			if creates == 2 {
				require.NoError(t, c.Tracker().Create(csrGVR, a.(k8stesting.CreateAction).GetObject(), ""))
				return true, nil, apierrors.NewTimeoutError("the write may have succeeded", 0)
			}
			return false, nil, nil
		})
		_, _, err := generateClientCertAndKey(t.Context(), c, opts)
		require.ErrorContains(t, err, "creating CSR kubernetes object")
		assert.Empty(t, csrNames(t, c), "the re-create's CSR is deleted (P9)")
	})

	t.Run("replaced after issuance: another key's CSR is left alone", func(t *testing.T) {
		other := others(t)
		api := newCSRServer(t, nil)
		api.issue = true
		issuedReads := 0
		api.onGet = func(s *csrServer, name string) {
			issuedReads++
			if issuedReads == 2 { // the wait saw the certificate, signup read it: now another signup takes the name
				s.put(other)
			}
		}
		_, _, err := generateClientCertAndKey(t.Context(), jsonClient(t, api.srv), opts)
		require.NoError(t, err, "nothing of ours to delete is not a failure")
		require.Equal(t, []string{other.Name}, api.names())
		assert.Equal(t, other.Spec.Request, api.csrs[other.Name].Spec.Request, "the other signup's CSR is kept")
		assert.Zero(t, api.deletes.Load(), "not even a delete is sent for a CSR that isn't ours")
	})

	t.Run("re-created between the read and the delete: the UID precondition keeps it", func(t *testing.T) {
		api := newCSRServer(t, nil)
		api.issue = true
		api.onDelete = func(*http.Request) {
			// The cleanup read ours; before its delete lands, another signup
			// replaces it. Even with our request bytes, only the UID tells them apart.
			api.mu.Lock()
			defer api.mu.Unlock()
			again := api.csrs[opts.username].DeepCopy()
			again.Spec.Username = "other"
			api.put(again)
		}
		_, _, err := generateClientCertAndKey(t.Context(), jsonClient(t, api.srv), opts)
		require.NoError(t, err, "a precondition that no longer holds means nothing of ours is left")
		require.Len(t, api.names(), 1, "the re-created CSR is kept")
		assert.Equal(t, "other", api.csrs[opts.username].Spec.Username, "the other signup's, untouched")
	})
}

// A delete the apiserver never answers holds the signup no longer than the
// cleanup's deadline (P4): the DELETE request itself carries the context.
func TestAHungDeleteIsBounded(t *testing.T) {
	old := csrCleanupTimeout
	csrCleanupTimeout = 300 * time.Millisecond
	t.Cleanup(func() { csrCleanupTimeout = old })

	api := newCSRServer(t, nil)
	api.issue = true
	api.onDelete = func(r *http.Request) { <-r.Context().Done() } // never answers
	start := time.Now()
	crt, _, err := generateClientCertAndKey(t.Context(), jsonClient(t, api.srv), opts)
	require.ErrorContains(t, err, "deleting the used CSR")
	assert.Empty(t, crt)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	// The certificate wait's first poll (certs' certificateWaitPollInternval,
	// 3s) comes before the cleanup; after it, only csrCleanupTimeout.
	const certificatePoll = 3 * time.Second
	assert.Less(t, time.Since(start), certificatePoll+2*time.Second, "bounded by csrCleanupTimeout")
}

// csrServer is an in-memory apiserver for CSRs: create, approval, get and
// delete. It never issues a certificate. onApproval runs when an approval
// arrives.
type csrServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	csrs    map[string]*certv1.CertificateSigningRequest
	uids    atomic.Int32
	deletes atomic.Int32
	// issue makes the signer issue every CSR as it is created, so the
	// certificate wait ends at its first poll.
	issue bool
	// onGet runs, under the lock, after a GET is answered; onDelete runs
	// before a DELETE is served and may block (it gets the request).
	onGet    func(s *csrServer, name string)
	onDelete func(r *http.Request)
}

// put stores csr under a fresh UID, as the apiserver does on create.
func (s *csrServer) put(csr *certv1.CertificateSigningRequest) {
	csr = csr.DeepCopy()
	csr.UID = types.UID(fmt.Sprintf("uid-%d", s.uids.Add(1)))
	s.csrs[csr.Name] = csr
}

func newCSRServer(t *testing.T, onApproval func()) *csrServer {
	t.Helper()
	const base = "/apis/certificates.k8s.io/v1/certificatesigningrequests"
	s := &csrServer{csrs: map[string]*certv1.CertificateSigningRequest{}}
	notFound := func(w http.ResponseWriter, name string) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"%s not found"}`, name)
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")
		name, sub, _ := strings.Cut(rest, "/")
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodDelete && s.onDelete != nil {
			s.onDelete(r) // outside the lock: it may block
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		write := func(code int, obj any) {
			b, err := json.Marshal(obj)
			require.NoError(t, err)
			w.WriteHeader(code)
			_, _ = w.Write(b)
		}
		switch {
		case r.Method == http.MethodPost && name == "":
			var csr certv1.CertificateSigningRequest
			require.NoError(t, json.Unmarshal(body, &csr))
			s.put(&csr)
			if s.issue {
				s.csrs[csr.Name].Status.Certificate = []byte("CERT")
			}
			write(http.StatusCreated, s.csrs[csr.Name])
		case r.Method == http.MethodPut && sub == "approval":
			if onApproval != nil {
				onApproval()
			}
			_, _ = w.Write(body)
		case r.Method == http.MethodGet && name != "":
			csr, ok := s.csrs[name]
			if !ok {
				notFound(w, name)
				return
			}
			write(http.StatusOK, csr)
			if s.onGet != nil {
				s.onGet(s, name)
			}
		case r.Method == http.MethodDelete && name != "":
			s.deletes.Add(1)
			csr, ok := s.csrs[name]
			if !ok {
				notFound(w, name)
				return
			}
			var opts metav1.DeleteOptions
			if len(body) > 0 {
				require.NoError(t, json.Unmarshal(body, &opts))
			}
			if p := opts.Preconditions; p != nil && p.UID != nil && *p.UID != csr.UID {
				w.WriteHeader(http.StatusConflict)
				_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Conflict","code":409,"message":"precondition failed: UID in precondition: %s, UID in object meta: %s"}`, *p.UID, csr.UID)
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
