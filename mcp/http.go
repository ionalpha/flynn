package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/ionalpha/flynn/ids"
)

// HTTPHandler serves the MCP streamable-HTTP transport on loopback. Each POST
// carries one message and gets one application/json reply; tools answer
// synchronously, so no event stream is opened. Requests run under base, which
// carries the governance bindings as for Serve, so a halt cancels an in-flight
// call. A non-empty token is required as a bearer token, so another local user
// cannot use the port.
func (s *Server) HTTPHandler(base context.Context, token string) http.Handler {
	if base == nil {
		base = context.Background()
	}
	return &httpHandler{srv: s, base: base, token: token, session: ids.New()}
}

type httpHandler struct {
	srv     *Server
	base    context.Context
	token   string
	session string
}

const mcpSessionHeader = "Mcp-Session-Id"

// ServeHTTP answers a notification with 202. GET gets 405 because there is no
// server stream, and DELETE gets 200 because there is no session state, so a
// probing client gets an answer instead of a hang.
func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only a native local client drives this endpoint. A browser Origin, or a
	// non-loopback Host from DNS rebinding, is refused first; the MCP spec
	// requires this defense. The bearer token is the second check.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if !isLoopbackHost(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if h.token != "" && r.Header.Get("Authorization") != "Bearer "+h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		http.Error(w, "streaming not supported", http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var in inbound
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeFrame(w, http.StatusBadRequest, response{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: codeParse, Message: "parse error: " + err.Error()},
		})
		return
	}

	// Either the run halting or the client giving up cancels the call.
	ctx, cancel := mergeCancel(h.base, r.Context())
	defer cancel()

	resp, write := h.srv.handle(ctx, in)
	if !write {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set(mcpSessionHeader, h.session)
	writeFrame(w, http.StatusOK, resp)
}

// writeFrame falls back to an internal-error frame with a null id when the
// result cannot marshal, so the client never gets an empty body.
func writeFrame(w http.ResponseWriter, status int, resp response) {
	frame, err := resp.frame()
	if err != nil {
		frame = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"encode failure"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(frame, '\n'))
}

// isLoopbackHost accepts localhost or a loopback IP, with or without a port. A
// rebinding request carries the attacker's domain in Host, so this refuses it
// even without an Origin. An empty Host is refused, since HTTP/1.1 requires one.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// mergeCancel returns a context done when either parent is done. Call cancel to
// release the AfterFunc.
func mergeCancel(base, req context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(base)
	stop := context.AfterFunc(req, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}
