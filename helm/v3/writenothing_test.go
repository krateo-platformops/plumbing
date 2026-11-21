package helm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"k8s.io/client-go/rest"
)

// Every option that only applies to a write is refused, named, before any request is sent.
func TestReconcile_WriteNothingRefusesWriteOnlyOptions(t *testing.T) {
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sent++; w.WriteHeader(500) }))
	defer srv.Close()
	hc, err := NewClient(&rest.Config{Host: srv.URL}, WithNamespace("default"))
	if err != nil {
		t.Fatal(err)
	}
	for name, set := range map[string]func(*helmconfig.ActionConfig){
		"Force":       func(a *helmconfig.ActionConfig) { a.Force = true },
		"Wait":        func(a *helmconfig.ActionConfig) { a.Wait = true },
		"WaitForJobs": func(a *helmconfig.ActionConfig) { a.WaitForJobs = true },
		"Atomic":      func(a *helmconfig.ActionConfig) { a.Atomic = true },
	} {
		ac := &helmconfig.ActionConfig{}
		set(ac)
		_, err := hc.Reconcile(context.Background(), "r", "oci://x", &helmconfig.UpgradeConfig{ActionConfig: ac, WriteNothing: true})
		if !errors.Is(err, helmconfig.ErrWriteNothingConflict) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s with WriteNothing: err = %v; want ErrWriteNothingConflict naming %s", name, err, name)
		}
	}
	if sent != 0 {
		t.Errorf("%d request(s) sent; a refused Reconcile must send none", sent)
	}
	// CONTROL: the same options without WriteNothing are not refused here (they reach the server).
	_, err = hc.Reconcile(context.Background(), "r", "oci://x", &helmconfig.UpgradeConfig{ActionConfig: &helmconfig.ActionConfig{Force: true}})
	if errors.Is(err, helmconfig.ErrWriteNothingConflict) || sent == 0 {
		t.Errorf("control: Force without WriteNothing: err=%v sent=%d; want it to reach the server", err, sent)
	}
}

type seenReq struct {
	method, dryRun, body string
}

func TestDryRunTransport_EveryNonReadIsADryRun(t *testing.T) {
	var mu sync.Mutex
	var seen []seenReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, seenReq{r.Method, r.URL.Query().Get("dryRun"), string(b)})
		mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := &http.Client{Transport: &dryRunTransport{next: http.DefaultTransport, log: &dryRunLog{}}}
	do := func(method, body string) error {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, srv.URL+"/api/v1/namespaces/default/configmaps/x?fieldManager=helm", rd)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if err := do(m, ""); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	if err := do(http.MethodDelete, `{"kind":"DeleteOptions","apiVersion":"v1","propagationPolicy":"Background"}`); err != nil {
		t.Fatal(err)
	}
	for i, s := range seen {
		wantDry := !isReadMethod(s.method)
		if (s.dryRun == "All") != wantDry {
			t.Errorf("request %d %s: dryRun query = %q, want dry=%v", i, s.method, s.dryRun, wantDry)
		}
	}
	last := seen[len(seen)-1]
	var opts map[string]any
	if err := json.Unmarshal([]byte(last.body), &opts); err != nil {
		t.Fatalf("DELETE body %q: %v", last.body, err)
	}
	if d, _ := opts["dryRun"].([]any); len(d) != 1 || d[0] != "All" || opts["propagationPolicy"] != "Background" {
		t.Errorf("DELETE body = %q; want dryRun [All] added and propagationPolicy kept", last.body)
	}
	if len(seen) != 7 {
		t.Errorf("server saw %d requests, want 7", len(seen))
	}

	// Fail closed: a DELETE body that is not JSON options is not sent at all.
	n := len(seen)
	if err := do(http.MethodDelete, "not json"); err == nil {
		t.Error("a DELETE with a non-JSON body was sent; it cannot be made a dry run and must be refused")
	}
	if len(seen) != n {
		t.Errorf("the refused DELETE reached the server")
	}
}

func TestDryRunTransport_RecordsAdmissionDenialsOnly(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"an admission webhook's denial", `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"admission webhook \"deny.example.org\" denied the request: dry runs are refused here","code":403}`, true},
		// CONTROL: an RBAC 403 is not a webhook's dry-run refusal.
		{"an RBAC denial", `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"configmaps \"x\" is forbidden: User \"u\" cannot patch resource \"configmaps\"","reason":"Forbidden","code":403}`, false},
		{"not a Status", `not json`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(tc.body))
		}))
		log := &dryRunLog{}
		c := &http.Client{Transport: &dryRunTransport{next: http.DefaultTransport, log: log}}
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/v1/namespaces/default/configmaps/x", strings.NewReader("{}"))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		srv.Close()
		if string(got) != tc.body {
			t.Errorf("%s: the caller got body %q, want it intact", tc.name, got)
		}
		err = log.err()
		if (err != nil) != tc.want {
			t.Errorf("%s: recorded=%v, want %v", tc.name, err != nil, tc.want)
		}
		if tc.want && (!errors.Is(err, helmconfig.ErrDryRunRefused) || !strings.Contains(err.Error(), "/configmaps/x") || !strings.Contains(err.Error(), "deny.example.org")) {
			t.Errorf("%s: err = %v; want ErrDryRunRefused naming the resource and the webhook", tc.name, err)
		}
	}
}
