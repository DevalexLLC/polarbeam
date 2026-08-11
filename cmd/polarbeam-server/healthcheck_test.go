package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"
)

func TestHealthURL(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		// Wildcard binds are not dialable as written; loopback is.
		{":8080", "https://127.0.0.1:8080/healthz"},
		{"0.0.0.0:8080", "https://127.0.0.1:8080/healthz"},
		{"[::]:8080", "https://127.0.0.1:8080/healthz"},
		// An explicit bind must be dialed as configured — loopback would
		// not reach it.
		{"10.0.0.5:9090", "https://10.0.0.5:9090/healthz"},
		{"127.0.0.1:8080", "https://127.0.0.1:8080/healthz"},
		{"[::1]:8080", "https://[::1]:8080/healthz"},
		// A scoped IPv6 literal binds fine, so it must probe fine: the
		// zone needs %-escaping or net/http rejects the URL outright
		// ("invalid URL escape") and a healthy server never passes.
		{"[fe80::1%eth0]:8080", "https://[fe80::1%25eth0]:8080/healthz"},
	}
	for _, c := range cases {
		got, err := healthURL(c.listen)
		if err != nil {
			t.Errorf("healthURL(%q): unexpected error: %v", c.listen, err)
			continue
		}
		if got != c.want {
			t.Errorf("healthURL(%q) = %q, want %q", c.listen, got, c.want)
		}
	}
	// A missing port is a config error, named as one.
	if _, err := healthURL("0.0.0.0"); err == nil {
		t.Error("healthURL(\"0.0.0.0\"): want error, got nil")
	} else if !strings.Contains(err.Error(), "listen.http") {
		t.Errorf("healthURL error should name listen.http, got %v", err)
	}
}

func TestProbeHealth(t *testing.T) {
	// httptest's TLS certificate is self-signed, exactly like the dev
	// dashboard cert — the probe must not verify it.
	ok := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte("ok\n"))
	}))
	defer ok.Close()
	if err := probeHealth(ok.URL+"/healthz", 5*time.Second, false); err != nil {
		t.Fatalf("probeHealth on a healthy server: %v", err)
	}

	// Non-200 must fail the check, not pass it silently.
	down := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	if err := probeHealth(down.URL+"/healthz", 5*time.Second, false); err == nil {
		t.Error("probeHealth on a 503: want error, got nil")
	}

	// A server that answers 200 and then wedges mid-body is exactly the
	// half-alive state a liveness probe exists to catch. The client
	// deadline covers the body read, so the stall surfaces there and
	// nowhere else — discarding that error would report this healthy.
	stalled := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer stalled.Close()
	if err := probeHealth(stalled.URL+"/healthz", 1*time.Second, false); err == nil {
		t.Error("probeHealth against a server stalled mid-body: want error, got nil")
	}

	// A dead listener fails rather than hanging.
	closed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := closed.URL
	closed.Close()
	if err := probeHealth(url+"/healthz", 2*time.Second, false); err == nil {
		t.Error("probeHealth against a closed listener: want error, got nil")
	}
}

// TestProbeHealthProxyProtocol mirrors the listen.proxy_protocol server: the
// listener REQUIREs a PROXY header before TLS, so the probe must send one —
// and a probe that does not (knob mismatch) must fail loudly, not hang.
func TestProbeHealthProxyProtocol(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	}))
	srv.Listener = &proxyproto.Listener{
		Listener: srv.Listener,
		ConnPolicy: func(proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			return proxyproto.REQUIRE, nil
		},
		ReadHeaderTimeout: time.Second,
	}
	srv.StartTLS()
	defer srv.Close()

	if err := probeHealth(srv.URL+"/healthz", 5*time.Second, true); err != nil {
		t.Fatalf("probeHealth with PROXY header against a REQUIRE listener: %v", err)
	}
	if err := probeHealth(srv.URL+"/healthz", 5*time.Second, false); err == nil {
		t.Error("probeHealth without PROXY header against a REQUIRE listener: want error, got nil")
	}
}
