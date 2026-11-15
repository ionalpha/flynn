package netguard

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// Proxy is a forward proxy that applies a [Policy] to a child process pointed at
// it through HTTP_PROXY, HTTPS_PROXY or ALL_PROXY. It checks the requested name
// against AllowHosts, then dials through [DialControl]. It tunnels CONNECT and
// forwards absolute-form HTTP. Serve it on a loopback listener from bindguard so
// only the local host can reach it.
type Proxy struct {
	policy Policy
	dialer *net.Dialer
}

// NewProxy returns a proxy that admits only what p allows.
func NewProxy(p Policy) *Proxy {
	return &Proxy{policy: p, dialer: Dialer(p)}
}

// hostAllowed takes the request authority, with or without a port.
func (px *Proxy) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return px.policy.AllowsHost(host)
}

// Handler tunnels CONNECT and relays every other request as a forward proxy.
// Serve it only on a loopback listener.
func (px *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			px.handleConnect(w, r)
			return
		}
		px.handleForward(w, r)
	})
}

// dialUpstream returns a fault.Forbidden error for a policy refusal, so the
// caller can answer 403 instead of 502.
func (px *Proxy) dialUpstream(ctx context.Context, addr string) (net.Conn, error) {
	return px.dialer.DialContext(ctx, "tcp", addr)
}

func policyRefused(err error) bool { return err != nil && fault.Classify(err) == fault.Forbidden }

// handleConnect answers 403 for a policy refusal and 502 for an unreachable
// target.
func (px *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	// Check the name first, so a name outside the allowlist is never looked up.
	if !px.hostAllowed(r.Host) {
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	}
	upstream, err := px.dialUpstream(r.Context(), r.Host)
	if err != nil {
		if policyRefused(err) {
			http.Error(w, "egress denied by policy", http.StatusForbidden)
			return
		}
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	splice(client, upstream)
}

func (px *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "proxy: expected an absolute-form request", http.StatusBadRequest)
		return
	}
	if !px.hostAllowed(r.URL.Host) {
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	}
	addr := r.URL.Host
	if r.URL.Port() == "" {
		addr = net.JoinHostPort(r.URL.Hostname(), "80")
	}
	upstream, err := px.dialUpstream(r.Context(), addr)
	if err != nil {
		if policyRefused(err) {
			http.Error(w, "egress denied by policy", http.StatusForbidden)
			return
		}
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	// An empty RequestURI makes Write emit origin form.
	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	if err := outReq.Write(upstream); err != nil {
		http.Error(w, "proxy: write upstream", http.StatusBadGateway)
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(upstream), outReq)
	if err != nil {
		http.Error(w, "proxy: read upstream", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// Serve runs the proxy on ln until ctx is cancelled. ln should be a loopback
// listener from bindguard.
func (px *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: px.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { //nolint:gosec // G118: the drain runs because ctx is already done; a fresh context bounds shutdown, the dead one cannot
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// splice copies both directions and returns when both are drained. Each side is
// half-closed once its source ends, so a half-closed peer still completes.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
