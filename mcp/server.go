// Package mcp serves the agent's tools to a Model Context Protocol client over
// stdio or streamable HTTP, and consumes tools from an MCP extension server
// ([Client]).
//
// Every tools/call goes through the same dispatch.Dispatcher as the native loop,
// so it is checked against the run's capability grant, containment level and
// brakes, and recorded on the spine. Listing a tool does not permit it. A denied
// client gets an error tool result. The server holds no grant or brake itself:
// the caller binds them on the context with capability.Into and brakes.Into.
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

// Server exposes a fixed set of [mission.Tool] values. One Serve call drives
// one connection; connections may follow one another.
type Server struct {
	dispatcher *dispatch.Dispatcher
	tools      map[string]mission.Tool
	order      []string // first-registration order, deduped
	scope      state.Scope
	goal       string
	info       Info
}

// Option configures a Server at construction.
type Option func(*Server)

// WithScope sets the scope tool calls are recorded under. The zero scope is
// global.
func WithScope(s state.Scope) Option { return func(sv *Server) { sv.scope = s } }

// WithGoal sets the goal id tool calls are recorded under. Empty means none.
func WithGoal(id string) Option { return func(sv *Server) { sv.goal = id } }

// WithInfo sets the identity reported in the initialize handshake.
func WithInfo(i Info) Option { return func(sv *Server) { sv.info = i } }

// NewServer serves tools through d. A later tool with a duplicate name replaces
// the earlier one; tools/list keeps first-registration order. A nil d becomes
// dispatch.New(), which allows everything and records nothing.
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

// Serve handles newline-delimited JSON-RPC from r until EOF or cancellation,
// both of which return nil. Every tool call runs under ctx and its bindings.
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

// readLoop reads in a goroutine so Serve can select on cancellation instead of
// blocking in a read. The first decode error, EOF included, ends it.
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

// handle returns false for a notification, which gets no reply.
func (s *Server) handle(ctx context.Context, in inbound) (response, bool) {
	if in.isNotification() {
		// notifications/initialized needs no action; others are ignored.
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

// callTool returns an rpcError only for malformed params. An unknown tool, a
// denial or a tool failure is a result with IsError set, so the model can
// adapt; the dispatcher has already recorded a denial.
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
		// A tool decoding into a struct needs an object, not null.
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

func (s *Server) reply(id json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

func (s *Server) fail(id json.RawMessage, e *rpcError) response {
	return response{JSONRPC: "2.0", ID: id, Error: e}
}

// toolTrust must match the mission executor, so a bridged call gets the
// containment level the tool gets on the native loop.
func toolTrust(t mission.Tool) sandbox.Trust {
	if tw, ok := t.(mission.TrustedWork); ok {
		return tw.WorkTrust()
	}
	return sandbox.TrustTrusted
}
