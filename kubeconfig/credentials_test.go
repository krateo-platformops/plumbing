package kubeconfig

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/krateo-platformops/plumbing/endpoints"
)

// The endpoint's credentials reach the *rest.Config NewClientConfig builds: a bearer token, a
// client certificate and key, or both, as clientcmd carries a kubeconfig user that has them. A
// token-only endpoint used to come out with no credential at all, because the kubeconfig user had no
// token field, and every request it made was anonymous.
//
// Failure messages name presence and lengths, never a credential's value.
func TestNewClientConfig_CarriesTheEndpointsCredentials(t *testing.T) {
	const token = "bearer-token-for-the-test"
	cert, key := []byte("client-certificate-bytes"), []byte("client-key-bytes")
	certData, keyData := base64.StdEncoding.EncodeToString(cert), base64.StdEncoding.EncodeToString(key)

	tests := []struct {
		name          string
		ep            endpoints.Endpoint
		wantToken     bool
		wantClientTLS bool
	}{
		{name: "token only", ep: endpoints.Endpoint{ServerURL: "https://kubernetes.example.com", Token: token}, wantToken: true},
		{name: "client certificate only", ep: endpoints.Endpoint{ServerURL: "https://kubernetes.example.com", ClientCertificateData: certData, ClientKeyData: keyData}, wantClientTLS: true},
		{name: "token and client certificate", ep: endpoints.Endpoint{ServerURL: "https://kubernetes.example.com", Token: token, ClientCertificateData: certData, ClientKeyData: keyData}, wantToken: true, wantClientTLS: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := NewClientConfig(context.Background(), tc.ep)
			if err != nil {
				t.Fatalf("NewClientConfig: %v", err)
			}
			if tc.wantToken {
				if rc.BearerToken != token {
					t.Errorf("BearerToken: present=%v, len=%d, want the endpoint's token (len %d)", rc.BearerToken != "", len(rc.BearerToken), len(token))
				}
			} else if rc.BearerToken != "" || rc.BearerTokenFile != "" {
				t.Errorf("a bearer credential on an endpoint without a token (BearerToken present=%v, BearerTokenFile present=%v)", rc.BearerToken != "", rc.BearerTokenFile != "")
			}
			if tc.wantClientTLS {
				if !bytes.Equal(rc.CertData, cert) || !bytes.Equal(rc.KeyData, key) {
					t.Errorf("client certificate: CertData equal=%v, KeyData equal=%v, want both the endpoint's", bytes.Equal(rc.CertData, cert), bytes.Equal(rc.KeyData, key))
				}
			} else if len(rc.CertData) != 0 || len(rc.KeyData) != 0 {
				t.Errorf("a client certificate on an endpoint without one (CertData len=%d, KeyData len=%d)", len(rc.CertData), len(rc.KeyData))
			}
			if rc.Username != "" || rc.Password != "" || rc.ExecProvider != nil || rc.AuthProvider != nil {
				t.Errorf("an unexpected credential: Username/Password present=%v, ExecProvider=%v, AuthProvider=%v", rc.Username != "" || rc.Password != "", rc.ExecProvider != nil, rc.AuthProvider != nil)
			}
		})
	}
}

// Marshal writes the token as the kubeconfig user's `token`, and leaves the key out when there is
// none, so a certificate endpoint's kubeconfig is what it was.
func TestMarshal_TokenIsTheUsersTokenAndOnlyWhenSet(t *testing.T) {
	userOf := func(t *testing.T, ep endpoints.Endpoint) map[string]any {
		t.Helper()
		b, err := Marshal(&ep)
		if err != nil {
			t.Fatal(err)
		}
		var kc struct {
			Users []struct {
				User map[string]any `json:"user"`
			} `json:"users"`
		}
		if err := json.Unmarshal(b, &kc); err != nil {
			t.Fatal(err)
		}
		if len(kc.Users) != 1 {
			t.Fatalf("%d users, want 1", len(kc.Users))
		}
		return kc.Users[0].User
	}
	const token = "bearer-token-for-the-test"
	if got, ok := userOf(t, endpoints.Endpoint{ServerURL: "https://k", Token: token})["token"]; !ok || got != token {
		t.Errorf("a token endpoint's user: token present=%v, equal=%v", ok, got == token)
	}
	if _, ok := userOf(t, endpoints.Endpoint{ServerURL: "https://k", ClientCertificateData: "Y2VydA==", ClientKeyData: "a2V5"})["token"]; ok {
		t.Error("a certificate endpoint's user has a token key")
	}
}
