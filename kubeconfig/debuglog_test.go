package kubeconfig

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"k8s.io/client-go/rest"
)

// At DEBUG (DEBUG=true), NewClientConfig's client logs each request as metadata only. The request
// it sees carries the endpoint's bearer token (the debug wrapper sits inside client-go's bearer
// round tripper), so a header or wire dump would put the credential in the log. The test proves the
// token was on the wire (the server checks it) and that the log line was written, then requires the
// log to hold neither the token, nor an Authorization header, nor the query.
func TestNewClientConfig_DebugLogHoldsNoCredential(t *testing.T) {
	const token = "bearer-token-for-the-debug-test"
	const queryValue = "query-value-for-the-debug-test"
	sawBearer := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBearer = r.Header.Get("Authorization") == "Bearer "+token
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=cookie-value-for-the-debug-test")
		_, _ = w.Write([]byte(`{"kind":"APIVersions"}`))
	}))
	defer srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	t.Setenv("DEBUG", "true")
	var logs bytes.Buffer
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithLogger(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	rc, err := NewClientConfig(ctx, endpoints.Endpoint{ServerURL: srv.URL, Token: token, CertificateAuthorityData: base64.StdEncoding.EncodeToString(ca)})
	if err != nil {
		t.Fatalf("NewClientConfig: an error of type %T", err)
	}
	hc, err := rest.HTTPClientFor(rc)
	if err != nil {
		t.Fatalf("HTTPClientFor: an error of type %T", err)
	}
	resp, err := hc.Get(srv.URL + "/api?labelSelector=" + url.QueryEscape(queryValue))
	if err != nil {
		t.Fatalf("GET: an error of type %T", err)
	}
	resp.Body.Close()
	if !sawBearer {
		t.Fatal("the server did not see the endpoint's bearer token: the test could not see a leak")
	}

	out := logs.String()
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "kubeconfig: request" {
			line = m
		}
	}
	if line == nil {
		t.Fatalf("no request line at DEBUG (%d bytes logged): the test could not see a leak", len(out))
	}
	if line["method"] != "GET" || line["path"] != "/api" || line["status"] != float64(200) {
		t.Errorf("the request line's metadata: method=%v path=%v status=%v", line["method"], line["path"], line["status"])
	}
	for what, s := range map[string]string{
		"the token": token, "an Authorization header": "Authorization", "a bearer value": "Bearer",
		"the query": queryValue, "a Set-Cookie value": "cookie-value-for-the-debug-test",
	} {
		if strings.Contains(out, s) {
			t.Errorf("the debug log holds %s", what)
		}
	}
}

// Marshal writes the endpoint's proxy URL as the cluster's proxy-url, and NewClientConfig's config
// uses it; without one there is no proxy-url key.
func TestNewClientConfig_CarriesTheProxyURL(t *testing.T) {
	rc, err := NewClientConfig(context.Background(), endpoints.Endpoint{ServerURL: "https://kubernetes.example.com", ProxyURL: "http://proxy.example.com:3128"})
	if err != nil {
		t.Fatalf("NewClientConfig: an error of type %T", err)
	}
	if rc.Proxy == nil {
		t.Fatal("no proxy on a config whose endpoint has a proxy URL")
	}
	req, _ := http.NewRequest("GET", "https://kubernetes.example.com/api", nil)
	u, err := rc.Proxy(req)
	if err != nil || u == nil || u.String() != "http://proxy.example.com:3128" {
		t.Errorf("the proxy: %v (err type %T)", u, err)
	}
	b, err := Marshal(&endpoints.Endpoint{ServerURL: "https://k"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "proxy-url") {
		t.Error("a proxy-url key for an endpoint without a proxy URL")
	}
}
