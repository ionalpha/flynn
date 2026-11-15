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

// maxClientMessageBytes caps one message in either direction, so a hostile
// extension cannot make the core buffer an unbounded reply. It matches the
// flynn-extensions harness cap.
const maxClientMessageBytes = 16 << 20

const clientReadBufferHint = 64 << 10

// ToolDesc is one tool from tools/list. Every field is untrusted extension data:
// the consumer namespaces Name and bounds and masks Description before either
// reaches a model or the spine, which guards against tool poisoning.
type ToolDesc struct {
	// Name is the server's name for the tool, before namespacing.
	Name string
	// Description is the model-facing text. Untrusted.
	Description string
	// InputSchema is nil when the server declares none.
	InputSchema json.RawMessage
}

// CallResult is a tools/call outcome. IsError marks a tool-level failure the
// model sees and adapts to; a transport or protocol failure is CallTool's error
// return instead. Text is untrusted and must be bounded and masked.
type CallResult struct {
	Text    string
	IsError bool
}

// ClientOption configures a Client at construction.
type ClientOption func(*Client)

// WithClientInfo sets the identity reported in the initialize handshake.
func WithClientInfo(i Info) ClientOption { return func(c *Client) { c.info = i } }

// Client speaks MCP JSON-RPC 2.0 to an extension server over its stdio pipes.
// It treats the extension as possibly compromised: reads are size-capped, each
// call ends at its context deadline, replies pair to requests by id, and a dead
// transport fails every pending and future call. A message the server
// originates (sampling, elicitation, a notification) is dropped unanswered, so
// an extension can never call back into flynn.
type Client struct {
	w    io.Writer
	info Info

	wmu sync.Mutex // so two requests never interleave on the wire

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan rawResponse
	closed  bool
	closeCh chan struct{}
	readErr error
}

type rawResponse struct {
	result json.RawMessage
	rpcErr *rpcError
}

// clientRequest with a nil ID is a notification.
type clientRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// clientResponse reads Method only so readLoop can drop a server-initiated
// message.
type clientResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// NewClient starts a client that reads replies from r and writes requests to w.
// The caller must Close it. Close leaves r and w open, because their owner (the
// sandbox process handle) closes them. A Client is safe for concurrent use.
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

// readLoop routes each reply to the caller waiting on its id and drops unknown
// or duplicate ids. When the stream ends or a message is oversized, it fails
// every pending and future call so no caller waits on a dead peer.
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
			// The request it was meant for times out on its own deadline.
			continue
		}
		if resp.Method != "" {
			// A server-initiated request or notification. Dropping it before id
			// matching also stops one that reuses a pending id from posing as
			// that request's reply.
			continue
		}
		id, ok := decodeResponseID(resp.ID)
		if !ok {
			continue // only numeric ids are ever sent
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

// deliver removes the pending entry, so a duplicate reply finds no waiter. The
// channel has capacity 1, so the send never blocks on a caller that gave up.
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

// fail is idempotent, so the read loop ending and Close may race.
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

// Close fails any in-flight call. It is idempotent and leaves the reader and
// writer open.
func (c *Client) Close() error {
	c.fail(errClosed)
	return nil
}

var errClosed = errors.New("mcp: client closed")

// call returns at the context deadline, so a silent peer cannot wedge the
// caller, and at once when the transport dies.
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

// write refuses a message over maxClientMessageBytes instead of sending it.
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

// Initialize performs the MCP handshake and returns the server's identity. Call
// it once before ListTools or CallTool. Any non-empty protocol version is
// accepted.
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
	// The spec expects this notification; it has no reply to check.
	_ = c.write(clientRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	return res.ServerInfo, nil
}

// ListTools returns every tool the server exposes, as untrusted [ToolDesc]
// values.
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

// CallTool invokes a tool by its un-namespaced name. A tool-level failure comes
// back in [CallResult.IsError]; the error return means a timeout, a dead peer
// or a malformed reply.
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

// joinTextBlocks drops non-text blocks, so a block type flynn does not render
// cannot carry content in unseen.
func joinTextBlocks(blocks []contentBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// decodeResponseID reports a string, null or absent id as unmatched, never
// coerced onto a waiting caller.
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
