// Package mcp serves a set of the agent's tools to a Model Context Protocol
// client over a JSON-RPC 2.0 stdio transport, so an external program (another
// agent's harness, an editor, any MCP client) can call the agent's tools without
// being handed the host directly.
//
// The point of the server is where a called tool runs: every tools/call is routed
// through the same dispatch waist as a native loop, so the caller's effects are
// admitted against the run's capability grant, gated by the containment level its
// trust requires, subject to the safety brakes, and recorded on the event spine.
// Presence in tools/list makes a tool reachable, never automatically permitted:
// authority is decided at the waist at call time, exactly as it is for the agent's
// own loop. A client that is denied sees an ordinary error tool result and can
// adapt, while the denial lands on the spine as a rejected action.
//
// The server is a pure protocol and routing layer. It holds no grant and no brake
// of its own: the governance bindings ride on the context passed to Serve (the
// caller binds the run's grant with capability.Into and the run id with
// brakes.Into, the same way the mission executor does before it dispatches), and
// the server propagates that context into every Govern call. So a host that wants
// a read-only session binds a narrow grant; a host that wants a kill-switch binds
// the brakes; the server needs no knowledge of either.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// Server exposes a fixed set of mission.Tools to one MCP client connection and
// governs every call through a dispatch waist. Construct it with NewServer and run
// it with Serve. It is safe to reuse across sequential connections, but a single
// Serve call drives one connection.
type Server struct {
	dispatcher *dispatch.Dispatcher
	tools      map[string]mission.Tool
	order      []string // tools/list order: registration order, deduped
	scope      state.Scope
	goal       string
	info       Info
}

// Option configures a Server at construction.
type Option func(*Server)

// WithScope sets the scope every governed tool call is attributed to on the spine,
// so a session's actions are located on the instance/project/workspace axis like
// any native action. The zero scope is the global scope.
func WithScope(s state.Scope) Option { return func(sv *Server) { sv.scope = s } }

// WithGoal sets the goal id every governed tool call runs under, so the actions a
// client drives are threaded onto the same goal as the run that hosts the server.
// Empty means the calls belong to no specific goal.
func WithGoal(id string) Option { return func(sv *Server) { sv.goal = id } }

// WithInfo sets the server identity reported in the initialize handshake. A zero
// Info reports a default name and an empty version.
func WithInfo(i Info) Option { return func(sv *Server) { sv.info = i } }

// NewServer builds a server that serves tools through d. Tools are keyed by name;
// a later tool with a duplicate name replaces an earlier one, and tools/list
// reports each name once in first-registration order. A nil dispatcher is
// replaced with a zero-config one (allow-all, discard sink), so the server is
// usable standalone, though a real host passes a governed dispatcher.
func NewServer(d *dispatch.Dispatcher, tools []mission.Tool, opts ...Option) *Server {
	if d == nil {
		d = dispatch.New()
	}
	s := &Server{
		dispatcher: d,
		tools:      make(map[string]mission.Tool, len(tools)),
		info:       Info{Name: "flynn", Version: ""},
	}
	for _, t := range tools {
		name := t.Def().Name
		if name == "" {
			continue
		}
		if _, seen := s.tools[name]; !seen {
			s.order = append(s.order, name)
		}
		s.tools[name] = t
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Serve reads newline-delimited JSON-RPC messages from r, dispatches each, and
// writes replies to w, until r reaches EOF or ctx is cancelled. It returns nil on
// a clean client disconnect (EOF) or context cancellation, and a wrapped error
// only on an unrecoverable transport failure. Governance bindings on ctx (the
// run's grant and brakes) are propagated into every tool call; a caller that wants
// a governed session binds them before calling Serve.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	msgs, errc := s.readLoop(ctx, r)
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return fault.Wrap(fault.Transient, "mcp_transport", err)
		case in, ok := <-msgs:
			if !ok {
				return nil
			}
			resp, write := s.handle(ctx, in)
			if !write {
				continue
			}
			frame, err := resp.frame()
			if err != nil {
				return fault.Wrap(fault.Transient, "mcp_write", err)
			}
			if _, err := w.Write(append(frame, '\n')); err != nil {
				return fault.Wrap(fault.Transient, "mcp_write", err)
			}
		}
	}
}

// readLoop decodes messages off r in a goroutine and delivers them on a channel, so
// the Serve loop can also select on ctx cancellation instead of blocking forever in
// a read. A decode error (including EOF on disconnect) is reported once on errc and
// ends the goroutine.
func (s *Server) readLoop(ctx context.Context, r io.Reader) (<-chan inbound, <-chan error) {
	msgs := make(chan inbound)
	errc := make(chan error, 1)
	go func() {
		defer close(msgs)
		dec := json.NewDecoder(bufio.NewReader(r))
		for {
			var in inbound
			if err := dec.Decode(&in); err != nil {
				errc <- err
				return
			}
			select {
			case msgs <- in:
			case <-ctx.Done():
				return
			}
		}
	}()
	return msgs, errc
}

// handle routes one inbound message to its handler and returns the response to
// write, or write=false for a notification (which gets no reply). An unknown
// method on a request is a method-not-found error; on a notification it is ignored.
func (s *Server) handle(ctx context.Context, in inbound) (response, bool) {
	if in.isNotification() {
		// The only notification the server expects is notifications/initialized,
		// which needs no action. Any other notification is ignored, per JSON-RPC.
		return response{}, false
	}
	switch in.Method {
	case "initialize":
		return s.reply(in.ID, s.initialize(in.Params)), true
	case "tools/list":
		return s.reply(in.ID, toolsListResult{Tools: s.toolDefs()}), true
	case "tools/call":
		res, rerr := s.callTool(ctx, in.Params)
		if rerr != nil {
			return s.fail(in.ID, rerr), true
		}
		return s.reply(in.ID, res), true
	case "ping":
		return s.reply(in.ID, struct{}{}), true
	default:
		return s.fail(in.ID, &rpcError{Code: codeMethodNotFound, Message: "unknown method: " + in.Method}), true
	}
}

// initialize negotiates the protocol version and reports the server's tool
// capability and identity. It echoes the client's requested version when present,
// so both sides settle on a version the client named.
func (s *Server) initialize(params json.RawMessage) initializeResult {
	version := protocolVersion
	if len(params) > 0 {
		var p initializeParams
		if err := json.Unmarshal(params, &p); err == nil && p.ProtocolVersion != "" {
			version = p.ProtocolVersion
		}
	}
	return initializeResult{
		ProtocolVersion: version,
		Capabilities:    serverCapabilities{Tools: &toolsCapability{ListChanged: false}},
		ServerInfo:      s.info,
	}
}

// toolDefs renders the exposed tools as wire declarations in list order. A tool
// that declares no input schema gets an empty-object schema, since MCP requires
// inputSchema to be an object.
func (s *Server) toolDefs() []toolDef {
	defs := make([]toolDef, 0, len(s.order))
	for _, name := range s.order {
		d := s.tools[name].Def()
		schema := d.InputSchema
		if len(schema) == 0 {
			schema = emptyObjectSchema
		}
		defs = append(defs, toolDef{Name: d.Name, Description: d.Description, InputSchema: schema})
	}
	return defs
}

// callTool runs one tools/call through the dispatch waist. A malformed request (bad
// params, missing name) is a protocol error and returns a non-nil rpcError. An
// unknown tool, a governance denial, or a tool failure is not a protocol error: it
// returns a tool result with IsError set so the calling model sees it and adapts,
// while a denial is already recorded on the spine by the waist.
func (s *Server) callTool(ctx context.Context, params json.RawMessage) (callResult, *rpcError) {
	var p callParams
	if err := json.Unmarshal(params, &p); err != nil {
		return callResult{}, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params: " + err.Error()}
	}
	if p.Name == "" {
		return callResult{}, &rpcError{Code: codeInvalidParams, Message: "tools/call requires a tool name"}
	}
	tool, ok := s.tools[p.Name]
	if !ok {
		return callResult{Content: textContent("unknown tool: " + p.Name), IsError: true}, nil
	}

	args := p.Arguments
	if len(args) == 0 {
		// A tool that decodes its input into a struct expects an object; an absent
		// arguments field becomes an empty object rather than null.
		args = json.RawMessage(`{}`)
	}

	var out string
	gerr := s.dispatcher.Govern(ctx,
		dispatch.Action{Name: p.Name, Scope: s.scope, Trust: toolTrust(tool), Goal: s.goal},
		func(ctx context.Context) (dispatch.Metering, error) {
			var err error
			out, err = tool.Invoke(ctx, args)
			return dispatch.Metering{}, err
		})
	if gerr != nil {
		return callResult{Content: textContent(gerr.Error()), IsError: true}, nil
	}
	return callResult{Content: textContent(out)}, nil
}

// reply builds a success response echoing id.
func (s *Server) reply(id json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

// fail builds an error response echoing id.
func (s *Server) fail(id json.RawMessage, e *rpcError) response {
	return response{JSONRPC: "2.0", ID: id, Error: e}
}

// toolTrust returns the trust level a tool's work carries: the level it declares
// through mission.TrustedWork, or sandbox.TrustTrusted for a tool that declares
// none. It mirrors the mission executor so a bridged call is gated at the same
// containment level the same tool would be on a native loop.
func toolTrust(t mission.Tool) sandbox.Trust {
	if tw, ok := t.(mission.TrustedWork); ok {
		return tw.WorkTrust()
	}
	return sandbox.TrustTrusted
}
