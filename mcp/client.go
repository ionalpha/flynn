package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ionalpha/flynn/fault"
)

// maxClientMessageBytes bounds a single JSON-RPC message the client will read from a
// peer. A hostile or broken extension server must not be able to force the core to
// buffer unbounded memory for one reply (OWASP MCP resource-exhaustion / DoS). It
// matches the flynn-extensions harness cap so a legitimate large tool result still
// fits while a runaway one is cut off with a transport error rather than an OOM.
const maxClientMessageBytes = 16 << 20

// clientReadBufferHint is the initial scan buffer the reader starts with; it grows up
// to maxClientMessageBytes as needed, so the common small message costs little and a
// large one is still bounded.
const clientReadBufferHint = 64 << 10

// ToolDesc is one tool as an extension server advertises it in tools/list: its name,
// the description the model reads to decide when to call it, and the JSON Schema for
// its arguments. It is the client-side view of the wire toolDef, kept separate so the
// consumer package need not reach into the server's unexported types. Everything in a
// ToolDesc is UNTRUSTED data authored by the extension: a consumer namespaces the name,
// and size-bounds and masks the description before it enters any model context or the
// signed spine (anti tool-poisoning).
type ToolDesc struct {
	// Name is the tool's own name as the server reports it, before any namespacing.
	Name string
	// Description is the model-facing text the server supplies. Untrusted; treat as data.
	Description string
	// InputSchema is the JSON Schema for the tool's arguments, or nil when the server
	// declares none (the consumer substitutes an empty-object schema).
	InputSchema json.RawMessage
}

// CallResult is the outcome of a tools/call: the tool's textual output and whether the
// server marked it a tool-level error. A tool-level error (IsError) is a normal result
// the calling model sees and adapts to, distinct from a transport or protocol failure,
// which surfaces as the error return of CallTool. Text is UNTRUSTED extension output;
// a consumer size-bounds and masks it before it reaches a model or the spine.
type CallResult struct {
	Text    string
	IsError bool
}

// ClientOption configures a Client at construction.
type ClientOption func(*Client)

// WithClientInfo sets the identity the client reports in the initialize handshake. A
// zero Info reports a default name and empty version.
func WithClientInfo(i Info) ClientOption { return func(c *Client) { c.info = i } }

// Client is the MCP consumer half: it speaks JSON-RPC 2.0 over a byte stream to an MCP
// server (an extension subprocess reached over its stdio pipes) and turns its advertised
// tools into calls. It is transport-agnostic on purpose: it reads from any io.Reader and
// writes to any io.Writer, so the sandbox launch that supplies the subprocess pipes and
// the client that talks over them stay decoupled, and the protocol logic is testable
// against an in-memory pair with no process at all.
//
// The client is hardened against a hostile or broken peer, since an extension is treated
// as potentially compromised: reads are size-bounded (a reply cannot exhaust memory),
// every request carries a deadline through its context (a peer that never answers cannot
// wedge a call), replies are matched to their request by id (a spurious or duplicate id
// is ignored, never mismatched to a waiting call), and a dead transport fails every
// in-flight and future call closed rather than hanging.
//
// The channel is strictly one-directional: flynn only ever initiates requests and only
// ever consumes their replies. A message the server ORIGINATES (a sampling or elicitation
// request, a notification) is dropped, never dispatched and never answered, so an
// extension can send tools and results but can never call back into flynn. This is the
// structural answer to "consuming MCP adds a surface": the surface is a client that speaks
// three methods and refuses everything the server tries to drive.
type Client struct {
	w    io.Writer
	info Info

	wmu sync.Mutex // serialises writes so two requests never interleave on the wire

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan rawResponse
	closed  bool
	closeCh chan struct{}
	readErr error
}

// rawResponse is one reply routed from the read loop to the waiting caller: the raw
// result bytes on success, or a populated error on failure.
type rawResponse struct {
	result json.RawMessage
	rpcErr *rpcError
}

// clientRequest is one JSON-RPC request the client sends. Notifications are sent with a
// nil id and never expect a reply.
type clientRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// clientResponse is one inbound JSON-RPC message the client reads. A reply sets exactly
// one of Result or Error and pairs to its request by ID. Method is read only to detect the
// one thing the client must refuse: a server-INITIATED request or notification. flynn is a
// pure MCP client, so a message carrying a Method is dropped rather than acted on (see
// readLoop); the field exists solely to recognise and discard it.
type clientResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// NewClient starts a client reading replies from r and writing requests to w, and
// launches the background read loop. The caller must call Close when done (and should
// defer it), which stops the read loop and fails any in-flight call; Close does not
// close r or w, whose lifetimes belong to whoever supplied them (the sandbox process
// handle). A client is safe for concurrent use.
func NewClient(r io.Reader, w io.Writer, opts ...ClientOption) *Client {
	c := &Client{
		w:       w,
		info:    Info{Name: "flynn", Version: ""},
		pending: make(map[int64]chan rawResponse),
		closeCh: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	go c.readLoop(r)
	return c
}

// readLoop scans newline-delimited replies from the peer, bounded in size, and routes
// each to the caller waiting on its id. An unknown or duplicate id is dropped rather
// than delivered to the wrong caller. When the stream ends or a message exceeds the
// bound, every pending and future call is failed with the transport error, so a caller
// waiting on a dead peer is released instead of hanging forever.
func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, clientReadBufferHint), maxClientMessageBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp clientResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// A single malformed line is not fatal to the session; the request it was
			// meant for will time out on its own deadline. Skip it and keep reading.
			continue
		}
		if resp.Method != "" {
			// A server-INITIATED request or notification (sampling, roots, elicitation, a
			// list-changed notice). flynn is a pure client: it consumes only replies to its
			// own requests and never serves a call the extension originates, so it never
			// answers this and never dispatches it. Dropping it here (before id-matching)
			// also means a message that reuses a pending request id cannot be mistaken for
			// that request's reply. The extension therefore cannot drive flynn in any way.
			continue
		}
		id, ok := decodeResponseID(resp.ID)
		if !ok {
			continue // a reply with no numeric id matches no request we sent
		}
		c.deliver(id, rawResponse{result: resp.Result, rpcErr: resp.Error})
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	if errors.Is(err, bufio.ErrTooLong) {
		err = fault.New(fault.Transient, "mcp_client_oversize",
			"mcp: extension reply exceeded the "+strconv.Itoa(maxClientMessageBytes)+"-byte limit")
	}
	c.fail(err)
}

// deliver hands a reply to the caller waiting on id, if one is still waiting. The
// pending entry is removed so a duplicate reply for the same id finds no waiter and is
// dropped. A non-blocking send guards against a caller that already gave up.
func (c *Client) deliver(id int64, resp rawResponse) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	ch <- resp
}

// fail marks the transport dead and releases every pending caller with err. It is
// idempotent, so the read loop ending and an explicit Close race harmlessly.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.readErr = err
	pending := c.pending
	c.pending = make(map[int64]chan rawResponse)
	close(c.closeCh)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rawResponse{rpcErr: &rpcError{Code: codeInternal, Message: err.Error()}}
	}
}

// Close stops the client and fails any in-flight call. It is idempotent and does not
// close the underlying reader or writer.
func (c *Client) Close() error {
	c.fail(errClosed)
	return nil
}

var errClosed = errors.New("mcp: client closed")

// call sends one request and waits for its reply or the context's deadline. The
// deadline is the anti-hang guarantee: a peer that never answers cannot wedge the
// caller, because ctx expiry abandons the pending entry and returns. A dead transport
// returns immediately via closeCh rather than waiting out the deadline.
func (c *Client) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rawResponse, 1)

	c.mu.Lock()
	if c.closed {
		err := c.readErr
		c.mu.Unlock()
		return nil, fault.Wrap(fault.Transient, "mcp_client_closed", err)
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(clientRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fault.Wrap(fault.Transient, "mcp_client_timeout", ctx.Err())
	case <-c.closeCh:
		return nil, fault.Wrap(fault.Transient, "mcp_client_closed", c.readErr)
	case resp := <-ch:
		if resp.rpcErr != nil {
			return nil, fault.New(fault.Transient, "mcp_client_rpc_error",
				fmt.Sprintf("mcp: %s: %s (code %d)", method, resp.rpcErr.Message, resp.rpcErr.Code))
		}
		return resp.result, nil
	}
}

// write serialises one message as a single newline-delimited line under the write lock,
// so concurrent callers never interleave bytes on the wire. It bounds the outgoing size
// with the same limit reads are held to, so a pathological argument payload is refused
// here rather than sent.
func (c *Client) write(req clientRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fault.Wrap(fault.Terminal, "mcp_client_encode", err)
	}
	if len(b) > maxClientMessageBytes {
		return fault.New(fault.Terminal, "mcp_client_oversize_send",
			"mcp: request exceeds the "+strconv.Itoa(maxClientMessageBytes)+"-byte limit")
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(b); err != nil {
		return fault.Wrap(fault.Transient, "mcp_client_write", err)
	}
	return nil
}

// Initialize performs the MCP handshake and returns the server's reported identity. It
// must be called once before ListTools or CallTool. The negotiated protocol version is
// checked for presence only; a server that answers the handshake at all is accepted, and
// the tools it later advertises are validated per call.
func (c *Client) Initialize(ctx context.Context) (Info, error) {
	params, err := json.Marshal(initializeParams{ProtocolVersion: protocolVersion})
	if err != nil {
		return Info{}, fault.Wrap(fault.Terminal, "mcp_client_encode", err)
	}
	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return Info{}, err
	}
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return Info{}, fault.Wrap(fault.Transient, "mcp_client_decode", err)
	}
	if res.ProtocolVersion == "" {
		return Info{}, fault.New(fault.Transient, "mcp_client_no_version",
			"mcp: server did not negotiate a protocol version")
	}
	// The spec expects an initialized notification after a successful handshake; send it
	// best-effort (a notification has no reply to wait on).
	_ = c.write(clientRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	return res.ServerInfo, nil
}

// ListTools asks the server for every tool it exposes. The names, descriptions, and
// schemas returned are untrusted extension data: the caller namespaces the names and
// bounds and masks the descriptions before any of it reaches a model or the spine.
func (c *Client) ListTools(ctx context.Context) ([]ToolDesc, error) {
	raw, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var res toolsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fault.Wrap(fault.Transient, "mcp_client_decode", err)
	}
	out := make([]ToolDesc, 0, len(res.Tools))
	for _, t := range res.Tools {
		out = append(out, ToolDesc(t))
	}
	return out, nil
}

// CallTool invokes one tool by its own (un-namespaced) name with the given arguments and
// returns the tool's textual output. A tool-level failure the server reports is returned
// as a CallResult with IsError set, not as an error; the error return is reserved for a
// transport or protocol failure (a timeout, a dead peer, a malformed reply). The returned
// text is untrusted and must be bounded and masked by the caller before use.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	params, err := json.Marshal(callParams{Name: name, Arguments: args})
	if err != nil {
		return CallResult{}, fault.Wrap(fault.Terminal, "mcp_client_encode", err)
	}
	raw, err := c.call(ctx, "tools/call", params)
	if err != nil {
		return CallResult{}, err
	}
	var res callResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return CallResult{}, fault.Wrap(fault.Transient, "mcp_client_decode", err)
	}
	return CallResult{Text: joinTextBlocks(res.Content), IsError: res.IsError}, nil
}

// joinTextBlocks concatenates the text of every text content block in order, the shape a
// flynn tool result takes. Non-text blocks are ignored: this consumer surfaces tools as
// text, and a block type it does not render must not smuggle content in unseen.
func joinTextBlocks(blocks []contentBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// decodeResponseID reads the numeric id the client assigns to its requests back from a
// reply. The client only ever sends integer ids, so a reply whose id is a string, null,
// or absent matches no outstanding request and is reported unmatched, never coerced onto
// a waiting caller.
func decodeResponseID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var id int64
	if err := json.Unmarshal(raw, &id); err != nil {
		return 0, false
	}
	return id, true
}
