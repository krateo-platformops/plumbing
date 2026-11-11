package signup

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	certv1 "k8s.io/api/certificates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
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
		crt, key, err := generateClientCertAndKey(c, opts)
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
		_, _, err := generateClientCertAndKey(c, opts)
		require.ErrorContains(t, err, "approving", "the original error is kept")
		assert.Empty(t, csrNames(t, c), "the refused CSR is deleted too")
	})

	t.Run("a stale CSR under the name is replaced, and then deleted", func(t *testing.T) {
		stale := &certv1.CertificateSigningRequest{}
		stale.Name = opts.username
		c := fake.NewClientset(stale)
		signer(c)
		_, _, err := generateClientCertAndKey(c, opts)
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
		_, _, err := generateClientCertAndKey(c, opts)
		require.ErrorContains(t, err, "waiting for CSR certificate")
		assert.Empty(t, csrNames(t, c), "the CSR is deleted")
	})

	t.Run("reading the certificate fails", func(t *testing.T) {
		c := fake.NewClientset()
		failingGet(c, 2) // the wait saw the certificate; the read after it fails
		crt, _, err := generateClientCertAndKey(c, opts)
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
		_, _, err := generateClientCertAndKey(c, opts)
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
		crt, key, err := generateClientCertAndKey(c, opts)
		require.ErrorContains(t, err, "deleting the used CSR")
		assert.Empty(t, crt, "nothing is returned for the caller to store")
		assert.Empty(t, key)
	})
}
