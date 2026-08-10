package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// cmdHealthcheck probes the dashboard's unauthenticated /healthz endpoint and
// exits non-zero if it does not answer 200 OK.
//
// It exists so the container HEALTHCHECK never forks a helper process. The
// previous check shelled out to BusyBox wget, which spawns /usr/bin/ssl_client
// for the TLS handshake and exits without reaping it; the orphan reparents to
// container PID 1, which is this binary — a Go process that never calls
// wait(2) — so every 30 s check leaked one permanent zombie onto the host's
// process table. A single-process Go probe has no child to leak, under any
// runtime (docker run, compose, podman, Kubernetes), with or without an init
// reaper.
//
// It also reads listen.http instead of assuming :8080, so a server configured
// onto another port stays checkable.
func cmdHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	// Kept below the container HEALTHCHECK timeout so the probe always
	// reports its own failure rather than being killed by the runtime.
	timeout := fs.Duration("timeout", 5*time.Second, "overall request deadline")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		return err
	}
	target, err := healthURL(cfg.Listen.HTTP)
	if err != nil {
		return err
	}
	return probeHealth(target, *timeout)
}

// probeHealth GETs target and reports anything short of a complete 200
// response — transport failure, non-200 status, or a body that stalls or
// truncates — as an error.
func probeHealth(target string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// The dashboard certificate is operator-supplied and its SANs
			// name the public hostname, not the loopback address dialed
			// here; in dev it is self-signed outright. This is a liveness
			// probe over loopback, not a trust decision — agent mTLS,
			// which is one, verifies in full.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
		},
	}
	resp, err := client.Get(target)
	if err != nil {
		return fmt.Errorf("healthcheck %s: %w", target, err)
	}
	defer resp.Body.Close()
	// Status first: a non-200 is reported for what it is, without waiting
	// out the deadline on a body nobody needs.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck %s: status %s", target, resp.Status)
	}
	// Draining is not politeness — the client deadline spans the body read,
	// so a server that answers 200 and then wedges or truncates surfaces
	// HERE and nowhere else. Discarding this error would report exactly
	// that half-alive state as healthy.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("healthcheck %s: reading response body: %w", target, err)
	}
	return nil
}

// healthURL turns a listen address into the /healthz URL to dial. A wildcard
// or empty host becomes loopback; an explicitly bound address is dialed as
// configured, because loopback would not reach it.
func healthURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen.http %q is not a host:port address: %w", listen, err)
	}
	if port == "" {
		return "", fmt.Errorf("listen.http %q has no port", listen)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	// Built through url.URL rather than concatenated: a scoped IPv6 literal
	// ([fe80::1%eth0]:8080) binds fine, but its raw "%eth0" reads as a
	// malformed percent-escape, and net/http would reject the URL before
	// ever dialing — a healthy server that could never pass its own check.
	// URL.String escapes the zone to %25eth0, which reparses correctly.
	u := url.URL{Scheme: "https", Host: net.JoinHostPort(host, port), Path: "/healthz"}
	return u.String(), nil
}
