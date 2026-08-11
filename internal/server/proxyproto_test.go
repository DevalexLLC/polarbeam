package server

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startTLSServer serves h over TLS on a maybeProxyProto-wrapped listener and
// returns its base URL plus a client that skips certificate verification
// (httptest's cert is self-signed, like the dev dashboard's).
func startTLSServer(t *testing.T, h http.Handler, proxyProto bool) (string, *http.Client) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = maybeProxyProto(srv.Listener, proxyProto)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	return srv.URL, client
}

// remoteAddrHandler echoes the host part of the connection's RemoteAddr.
var remoteAddrHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	fmt.Fprint(w, host)
})

func TestMaybeProxyProtoAdoptsHeaderSource(t *testing.T) {
	url, _ := startTLSServer(t, remoteAddrHandler, true)

	// Raw client: PROXY v1 header first (as nginx sends it), then TLS.
	addr := strings.TrimPrefix(url, "https://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "PROXY TCP4 203.0.113.7 203.0.113.1 40000 443\r\n"); err != nil {
		t.Fatal(err)
	}
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake after PROXY header: %v", err)
	}
	req := "GET / HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(tlsConn, req); err != nil {
		t.Fatal(err)
	}
	resp, err := io.ReadAll(tlsConn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(resp), "203.0.113.7") {
		t.Fatalf("handler did not see the PROXY header source; response:\n%s", resp)
	}
}

func TestMaybeProxyProtoRequiresHeader(t *testing.T) {
	url, client := startTLSServer(t, remoteAddrHandler, true)

	// No PROXY header: REQUIRE must reject the connection — the TLS
	// handshake fails instead of the limiter silently keying on a
	// spoofable or absent address.
	if _, err := client.Get(url); err == nil {
		t.Fatal("plain TLS request against a REQUIRE listener: want error, got nil")
	}
}

func TestMaybeProxyProtoDisabledPassthrough(t *testing.T) {
	url, client := startTLSServer(t, remoteAddrHandler, false)

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("disabled wrapper must leave the listener untouched: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "127.0.0.1") && !strings.HasPrefix(string(body), "::1") {
		t.Fatalf("expected loopback RemoteAddr, got %q", body)
	}
}
