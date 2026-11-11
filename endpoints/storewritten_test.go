package endpoints

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

// fakeSecrets answers Secret writes as the apiserver does: a create of an
// existing Secret 409, else the stored object with a uid, 201 for a create and
// 200 for an update. fail answers every write with that status instead.
func fakeSecrets(t *testing.T, exists bool, fail int) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method)
		w.Header().Set("Content-Type", "application/json")
		if fail != 0 {
			w.WriteHeader(fail)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sec := corev1.Secret{}
		require.NoError(t, json.Unmarshal(body, &sec))
		if r.Method == http.MethodPost && exists {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"AlreadyExists","code":409}`)
			return
		}
		sec.UID, sec.ResourceVersion = "uid-1", "5"
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		out, _ := json.Marshal(sec)
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// StoreWritten reports the request that stored the Secret: the create, or, for
// an existing Secret, the update after the create's 409, never the 409 itself.
func TestStoreWrittenReportsTheWrite(t *testing.T) {
	ep := Endpoint{Username: "authn", ServerURL: "https://k", ClientCertificateData: "c", ClientKeyData: "k"}
	for _, tc := range []struct {
		name   string
		exists bool
		want   Written
		calls  []string
	}{
		{"new", false, Written{Namespace: "ns", Name: "authn-clientconfig", UID: "uid-1", ResourceVersion: "5", Verb: "create", StatusCode: 201}, []string{"POST"}},
		{"existing", true, Written{Namespace: "ns", Name: "authn-clientconfig", UID: "uid-1", ResourceVersion: "5", Verb: "update", StatusCode: 200}, []string{"POST", "PUT"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := fakeSecrets(t, tc.exists, 0)
			got, err := StoreWritten(context.Background(), &rest.Config{Host: srv.URL}, "ns", ep)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.calls, *calls)
		})
	}

	t.Run("a failed write names the Secret, its verb and status, no uid", func(t *testing.T) {
		srv, _ := fakeSecrets(t, false, http.StatusForbidden)
		got, err := StoreWritten(context.Background(), &rest.Config{Host: srv.URL}, "ns", ep)
		require.Error(t, err)
		assert.Equal(t, Written{Namespace: "ns", Name: "authn-clientconfig", Verb: "create", StatusCode: 403}, got)
	})
}
