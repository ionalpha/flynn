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

// Proxy is a forward egress proxy that authorizes every upstream connection through a
// Policy, so a child process pointed at it (HTTP_PROXY / HTTPS_PROXY / ALL_PROXY) gets
// the same default-deny egress control the agent's own dials get. It is how the one
// egress policy engine governs a process whose own code we do not control: the child
// dials the proxy, the proxy enforces the policy after DNS resolution (rebinding-safe,
// via the same DialControl as Client and Dialer), and a denied destination is refused
// before any connection is made.
//
// It speaks the standard proxy protocol: CONNECT to tunnel TLS (and any other TCP a
// client opens through CONNECT), and absolute-form requests to forward plain HTTP. It
// holds no listener of its own; a caller serves it on a loopback listener (through
// bindguard), so the proxy is reachable only on the host it runs on.
type Proxy struct {
	dialer *net.Dialer
}

// NewProxy returns a proxy that admits only what p allows. Upstream connections are
// dialed through a policy-enforcing dialer, so the policy is applied at connect time on
// the resolved address (the same DialControl Client and Dialer use).
func NewProxy(p Policy) *Proxy {
	return &Proxy{dialer: Dialer(p)}
}

// Handler returns the proxy's HTTP handler: CONNECT requests are tunneled, everything
// else is treated as a forward-proxy request and relayed. Mount it on an http.Server
// served on a loopback listener.
func (px *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			px.handleConnect(w, r)
			return
		}
		px.handleForward(w, r)
	})
}

// dialUpstream dials addr through the policy-enforcing dialer. A policy refusal surfaces
// as a Forbidden error (see policyRefused), so the caller can answer 403 (denied) versus
// 502 (unreachable).
func (px *Proxy) dialUpstream(ctx context.Context, addr string) (net.Conn, error) {
	return px.dialer.DialContext(ctx, "tcp", addr)
}

// policyRefused reports whether err is a policy refusal (Forbidden) rather than a
// transport failure, so a blocked destination is distinguishable from an unreachable one.
func policyRefused(err error) bool { return err != nil && fault.Classify(err) == fault.Forbidden }

// handleConnect tunnels a CONNECT request: it dials the target through the policy and,
// on success, splices the client and upstream connections byte-for-byte. A policy
// refusal is a 403; an unreachable target is a 502.
func (px *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
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

// handleForward relays a plain-HTTP forward-proxy request. It dials the origin through
// the policy and writes the request out, then copies the response back, so a denied
// origin never receives the request.
func (px *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "proxy: expected an absolute-form request", http.StatusBadRequest)
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

	// Write the request to the origin in origin form (strip the proxy scheme/host), then
	// stream the response back verbatim.
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

// Serve runs the proxy on ln until ctx is cancelled, then shuts it down. ln should be a
// loopback listener (opened through bindguard) so the proxy is reachable only locally.
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

// splice copies bytes in both directions between two connections until either side
// closes, then returns. Each direction is closed for writing once its source is
// drained so a half-closed peer still completes.
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
