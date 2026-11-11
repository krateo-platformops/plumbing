//go:build js && wasm

package getter

import (
	"context"
	"os"
	"testing"
)

// Run by dialguard's TestPlumbingClientsRefuseRedirectsOnJS against a redirecting server: on
// js/wasm fetch would follow the hop without the credential strip on a cross-host redirect, so it
// must be refused.
func TestJSRefusesRedirects(t *testing.T) {
	url := os.Getenv("PLUMBING_JS_REDIRECT_URL")
	if url == "" {
		t.Skip("run by dialguard's TestPlumbingClientsRefuseRedirectsOnJS")
	}
	_, _, err := Get(context.Background(), url)
	if err == nil {
		t.Fatal("a redirect was followed")
	}
	t.Logf("refused: %v", err)
}
