//go:build js && wasm

package request

import (
	"os"
	"testing"

	"github.com/krateo-platformops/plumbing/endpoints"
)

// Run by dialguard's TestPlumbingClientsRefuseRedirectsOnJS against a redirecting server: on
// js/wasm fetch would follow the hop without this package's origin guards, so it must be refused.
func TestJSRefusesRedirects(t *testing.T) {
	url := os.Getenv("PLUMBING_JS_REDIRECT_URL")
	if url == "" {
		t.Skip("run by dialguard's TestPlumbingClientsRefuseRedirectsOnJS")
	}
	c, err := HTTPClientForEndpoint(&endpoints.Endpoint{ServerURL: url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(url)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("a redirect was followed: status %d", resp.StatusCode)
	}
	t.Logf("refused: %v", err)
}
