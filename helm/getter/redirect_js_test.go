//go:build js && wasm

package getter

import (
	"context"
	"net/url"
	"os"
	"testing"
)

// Run by dialguard's TestPlumbingClientsKeepTheFetchPolicyOnJS against a redirecting server: on
// js/wasm fetch would follow the hop without the credential strip on a cross-host redirect, so it
// must be refused.
func TestJSRefusesRedirects(t *testing.T) {
	url := os.Getenv("PLUMBING_JS_REDIRECT_URL")
	if url == "" {
		t.Skip("run by dialguard's TestPlumbingClientsKeepTheFetchPolicyOnJS")
	}
	_, _, err := Get(context.Background(), url)
	if err == nil {
		t.Fatal("a redirect was followed")
	}
	t.Logf("refused: %v", err)
}

// The OCI getter, likewise: its registry client must take fetch and refuse the hop.
func TestJSRefusesRedirectsOCI(t *testing.T) {
	raw := os.Getenv("PLUMBING_JS_REDIRECT_URL")
	if raw == "" {
		t.Skip("run by dialguard's TestPlumbingClientsKeepTheFetchPolicyOnJS")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	// A 127.0.0.1 registry is spoken to in plain HTTP (oci.go).
	_, _, err = Get(context.Background(), "oci://"+u.Host+"/start/chart", WithVersion("1.0.0"))
	if err == nil {
		t.Fatal("a redirect was followed")
	}
	t.Logf("refused: %v", err)
}
