package mcp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ionalpha/flynn/ids"
)

// HTTPHandler adapts the server to the Model Context Protocol streamable-HTTP
// transport, so a client that connects to a URL rather than spawning a subprocess
// (an in-process loopback bridge is the motivating case) drives the same governed
// tools over one HTTP endpoint. Each POST carries one JSON-RPC message and gets its
// reply as a single application/json body; the tools here answer synchronously, so
// the handler never needs to open a server-sent-event stream.
//
// base is the governed context the handler dispatches under: the caller binds the
// run's capability grant with capability.Into and the run id with brakes.Into
// before constructing the handler, exactly as for the stdio Serve path, and every
// request is served on a context derived from base so a run-level halt or shutdown
// cancels an in-flight call. token, when non-empty, is a bearer token the client
// must present, so a loopback port a co-tenant could also reach is not open to it.
func (s *Server) HTTPHandler(base context.Context, token string) http.Handler {
	if base == nil {
		base = context.Background()
	}
	return &httpHandler{srv: s, base: base, token: token, session: ids.New()}
}

// httpHandler serves one Server over streamable HTTP.
type httpHandler struct {
	srv     *Server
	base    context.Context
	token   string
	session string
}

// mcpSessionHeader carries the server-assigned session id, echoed by the client on
// subsequent requests, per the streamable-HTTP transport.
const mcpSessionHeader = "Mcp-Session-Id"

// ServeHTTP implements the transport. POST carries a JSON-RPC message and returns
// its reply (or 202 for a notification). GET and DELETE are answered minimally: this
// endpoint offers no server-initiated stream and keeps no per-session state to tear
// down, so a client that probes them gets a definite answer rather than a hang.
func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.token != "" && r.Header.Get("Authorization") != "Bearer "+h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		// No server-initiated stream is offered; the client drives every exchange with a
		// POST, so a GET for an event stream is declined.
		http.Error(w, "streaming not supported", http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		// The session holds no durable per-connection state, so an explicit teardown is
		// acknowledged and nothing needs releasing.
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var in inbound
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, response{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: codeParse, Message: "parse error: " + err.Error()},
		})
		return
	}

	// Serve on a context derived from the governed base so the run's grant and brake
	// apply, while the request's own cancellation still aborts a call the client gave
	// up on. The base cancelling (a halt or shutdown) cancels the request too.
	ctx, cancel := mergeCancel(h.base, r.Context())
	defer cancel()

	resp, write := h.srv.handle(ctx, in)
	if !write {
		// A notification gets no body; acknowledge receipt.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set(mcpSessionHeader, h.session)
	writeJSON(w, http.StatusOK, resp)
}

// writeJSON encodes v as an application/json body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// mergeCancel returns a context that is done when either parent is done, so a
// governed base (the run's halt/shutdown) and a per-request cancellation both abort
// the work. The returned cancel must be called to release the watcher goroutine.
func mergeCancel(base, req context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(base)
	stop := context.AfterFunc(req, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}
