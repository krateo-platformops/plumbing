//go:build !(js && wasm)

package dialguard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Off js/wasm, RoundTripper is the round-tripper it is given: Go follows redirects itself, so
// CheckRedirect and the credential round-trippers see every hop.
func TestRoundTripperIsTheIdentityOnServers(t *testing.T) {
	rt := Transport()
	if got := RoundTripper(rt); got != http.RoundTripper(rt) {
		t.Errorf("RoundTripper wrapped the transport on a server build: %T", got)
	}
}

// On js/wasm every plumbing client refuses a redirect, so no request reaches where it points:
// dialguard.Client (the JWKS key source, http/util), http/request and helm/getter. This runs the
// js tests for real under Node against a server here that redirects, in both of net/http's modes:
//
//   - fetch, as in a browser: through an exec wrapper whose argv0 isn't "node", which is what makes
//     net/http use Node's real fetch (roundtrip_js.go, jsFetchDisabled). Every client must send the
//     request and never reach the redirect's target.
//   - Go's in-process network, as under plain Node (go_js_wasm_exec): RoundTripper must be the
//     identity there, since Go follows redirects itself, CheckRedirect included.
//
// Without Node it skips, unless PLUMBING_REQUIRE_JS is set (CI sets it), so it can't pass by not
// running where it matters.
func TestPlumbingClientsRefuseRedirectsOnJS(t *testing.T) {
	gorootOut, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	wasmDir := filepath.Join(strings.TrimSpace(string(gorootOut)), "lib", "wasm")
	execNode := filepath.Join(wasmDir, "go_js_wasm_exec")
	_, nodeErr := exec.LookPath("node")
	_, execErr := os.Stat(execNode)
	if nodeErr != nil || execErr != nil {
		if os.Getenv("PLUMBING_REQUIRE_JS") != "" {
			t.Fatalf("PLUMBING_REQUIRE_JS is set but js/wasm tests can't run: node: %v, %s: %v", nodeErr, execNode, execErr)
		}
		t.Skip("node or go_js_wasm_exec not available; set PLUMBING_REQUIRE_JS to make this fail instead")
	}
	// As go_js_wasm_exec, with an argv0 that doesn't start with "node", so net/http uses fetch.
	execFetch := filepath.Join(t.TempDir(), "go_js_wasm_exec_fetch")
	script := "#!/usr/bin/env bash\nexec -a gojs-fetch node --stack-size=8192 " +
		strconvQuote(filepath.Join(wasmDir, "wasm_exec_node.js")) + ` "$@"` + "\n"
	if err := os.WriteFile(execFetch, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	var starts, targets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/start"): // helm/getter asks for <url>/index.yaml
			starts.Add(1)
			http.Redirect(w, r, "/target", http.StatusFound)
		case r.URL.Path == "/target":
			targets.Add(1)
			_, _ = w.Write([]byte("reached"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	run := func(t *testing.T, mode, execPath, pattern string, pkgs ...string) string {
		t.Helper()
		cmd := exec.Command("go", append([]string{"test", "-count=1", "-v", "-exec", execPath, "-run", pattern}, pkgs...)...)
		cmd.Dir = ".."
		cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "PLUMBING_JS_MODE="+mode,
			"PLUMBING_JS_REDIRECT_URL="+srv.URL+"/start")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the js/wasm tests failed: %v\n%s", err, out)
		}
		if strings.Contains(string(out), "no tests to run") || strings.Contains(string(out), "--- SKIP") {
			t.Fatalf("a js/wasm test didn't run:\n%s", out)
		}
		return string(out)
	}

	// One package at a time, so each must send its own request to the redirecting URL: a client
	// that fails for any other reason first proves nothing.
	for _, pkg := range []string{"./dialguard", "./http/request", "./helm/getter"} {
		t.Run("fetch, as in a browser: "+pkg, func(t *testing.T) {
			before := starts.Load()
			out := run(t, "fetch", execFetch, "^(TestJSRefusesRedirects|TestRoundTripperRefusesRedirectsOnJS)$", pkg)
			if starts.Load() == before {
				t.Fatalf("%s never requested the redirecting URL, so its refusal proves nothing:\n%s", pkg, out)
			}
			if n := targets.Load(); n != 0 {
				t.Fatalf("%s followed the redirect: the target was reached %d times\n%s", pkg, n, out)
			}
			if strings.Contains(out, "invalid header field name") {
				t.Fatalf("the redirect header reached net/http's own path instead of fetch:\n%s", out)
			}
		})
	}

	t.Run("Go's network, as under plain Node", func(t *testing.T) {
		run(t, "node", execNode, "^TestRoundTripperRefusesRedirectsOnJS$", "./dialguard")
	})
}

// strconvQuote single-quotes s for bash.
func strconvQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
