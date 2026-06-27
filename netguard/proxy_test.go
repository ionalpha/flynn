package netguard

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

// loopbackOnly is a policy that admits loopback (where httptest servers bind) and
// nothing else, so the tests exercise allow and deny without reaching the real network.
func loopbackOnly() Policy {
	return Policy{Allow: []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"),
		netip.MustParsePrefix("::1/128"),
	}}
}

// startProxy serves a proxy with policy p on a loopback listener and returns its URL and
// a stop func. The listener is opened directly (tests are exempt from the bind gate).
func startProxy(t *testing.T, p Policy) (*url.URL, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	px := NewProxy(p)
	done := make(chan struct{})
	go func() { _ = px.Serve(ctx, ln); close(done) }()
	u, _ := url.Parse("http://" + ln.Addr().String())
	return u, func() { cancel(); <-done }
}

func clientThrough(proxy *url.URL) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxy),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test upstream uses a self-signed cert
		},
	}
}

func TestProxyForwardsAllowedHTTP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello-from-upstream")
	}))
	defer upstream.Close()

	proxyURL, stop := startProxy(t, loopbackOnly())
	defer stop()

	resp, err := clientThrough(proxyURL).Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET via proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-from-upstream" {
		t.Errorf("body = %q, want hello-from-upstream", body)
	}
}

func TestProxyTunnelsAllowedHTTPS(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "secure-hello")
	}))
	defer upstream.Close()

	proxyURL, stop := startProxy(t, loopbackOnly())
	defer stop()

	resp, err := clientThrough(proxyURL).Get(upstream.URL) // https -> CONNECT tunnel
	if err != nil {
		t.Fatalf("HTTPS GET via proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "secure-hello" {
		t.Errorf("body = %q, want secure-hello", body)
	}
}

func TestProxyDeniesHTTPByPolicy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "should-not-reach")
	}))
	defer upstream.Close()

	// DenyAll admits nothing, including loopback.
	proxyURL, stop := startProxy(t, DenyAll())
	defer stop()

	resp, err := clientThrough(proxyURL).Get(upstream.URL)
	if err != nil {
		t.Fatalf("request should reach the proxy (and be refused there): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied HTTP status = %d, want 403", resp.StatusCode)
	}
}

func TestProxyDeniesHTTPSByPolicy(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	proxyURL, stop := startProxy(t, DenyAll())
	defer stop()

	// A denied CONNECT makes the tunnel fail, so the client's request errors out rather
	// than returning a response: the connection is refused before it is established.
	resp, err := clientThrough(proxyURL).Get(upstream.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Error("a denied HTTPS CONNECT must fail, not tunnel")
	}
}
