// Package signalcli adapts the Signal messenger to the inbox Source and Sink ports
// over JSON-RPC to a signal-cli daemon. signal-cli is a separate, operator-supplied
// process, linked to an existing Signal account as a secondary device, that serves
// newline-delimited JSON-RPC on a loopback TCP port. This package never embeds
// signal-cli and dials only that loopback daemon.
//
// Each request carries a unique id and waits for its matching response, so a reply is
// correlated to its call and a send is never lost behind another.
package signalcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/netguard"
)

const name = "signal"

// reconnectBackoff paces re-dialing the daemon after the connection drops.
const reconnectBackoff = 2 * time.Second

// loopbackPolicy permits only loopback addresses, so the egress gate blocks the client
// from being pointed anywhere but the local daemon.
var loopbackPolicy = netguard.Policy{Allow: []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}}

var errNotConnected = errors.New("signalcli: not connected to the daemon")

// Client is a Signal source and sink backed by a signal-cli JSON-RPC daemon.
type Client struct {
	addr    string
	account string
	dial    func(ctx context.Context, network, address string) (net.Conn, error)
	clk     clock.Timing

	mu      sync.Mutex
	enc     *json.Encoder
	nextID  uint64
	pending map[uint64]chan rpcResult
}

// Option configures a Client.
type Option func(*Client)

// WithAccount sets the Signal account (the linked +number) to act as. Only a daemon
// that serves several accounts needs it.
func WithAccount(a string) Option {
	return func(c *Client) { c.account = a }
}

// WithClock sets the clock that paces reconnect backoff, so a test drives re-dial
// timing with a Manual clock instead of sleeping.
func WithClock(clk clock.Timing) Option {
	return func(c *Client) {
		if clk != nil {
			c.clk = clk
		}
	}
}

// New builds a client for the signal-cli daemon at a loopback TCP address (for example
// 127.0.0.1:7583).
func New(tcpAddr string, opts ...Option) (*Client, error) {
	if tcpAddr == "" {
		return nil, errors.New("signalcli: empty daemon address")
	}
	c := &Client{
		addr:    tcpAddr,
		dial:    netguard.Dialer(loopbackPolicy).DialContext,
		clk:     clock.System{},
		pending: make(map[uint64]chan rpcResult),
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Name identifies the source and its paired sink.
func (c *Client) Name() string { return name }

// Receive streams each incoming text message as an inbox.Spec, reconnecting after a
// dropped connection, until ctx is cancelled, then closes the channel. The Spec's
// Source is left for the ingester to stamp.
func (c *Client) Receive(ctx context.Context) (<-chan inbox.Spec, error) {
	out := make(chan inbox.Spec)
	go func() {
		defer close(out)
		inbox.ReceiveLoop(ctx, reconnectBackoff, c.clk, func(ctx context.Context) error {
			return c.session(ctx, out)
		})
	}()
	return out, nil
}

// session runs one connection until it ends or ctx is cancelled. It returns non-nil on
// a connection fault, so the caller reconnects, and nil on cancellation.
func (c *Client) session(ctx context.Context, out chan<- inbox.Spec) error {
	conn, err := c.dial(ctx, "tcp", c.addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	c.mu.Lock()
	c.enc = json.NewEncoder(conn)
	c.mu.Unlock()
	defer c.teardown()

	// Close the connection when ctx is cancelled so the blocking read returns.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	// Best effort: a daemon started with --receive-mode=on-start already streams, and
	// one without the method errors back, which is ignored.
	_ = c.notify("subscribeReceive")

	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(ctx, line, out)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// dispatch routes one JSON-RPC line: a response wakes its waiting caller, a "receive"
// notification becomes an inbox entry, and anything else is ignored.
func (c *Client) dispatch(ctx context.Context, line []byte, out chan<- inbox.Spec) {
	var msg struct {
		ID     *uint64         `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return // not a JSON-RPC frame; ignore
	}
	if msg.ID != nil {
		c.complete(*msg.ID, rpcResult{result: msg.Result, err: msg.Error.err()})
		return
	}
	if msg.Method != "receive" {
		return
	}
	if spec, ok := specFromParams(msg.Params); ok {
		select {
		case out <- spec:
		case <-ctx.Done():
		}
	}
}

// Send delivers a reply: a +number goes to that recipient and any other conversation
// id is a group id. It waits for the matching response, so the reply is confirmed.
func (c *Client) Send(ctx context.Context, conversation, text string) error {
	_, err := c.call(ctx, "send", sendParams(conversation, text, c.account))
	return err
}

// sendParams treats a +digits conversation as a direct recipient and anything else as
// a group id, and includes the account only when set.
func sendParams(conversation, text, account string) map[string]any {
	params := map[string]any{"message": text}
	if strings.HasPrefix(conversation, "+") {
		params["recipient"] = []string{conversation}
	} else {
		params["groupId"] = conversation
	}
	if account != "" {
		params["account"] = account
	}
	return params
}

// call sends a request and waits for its response. It registers the id before writing,
// so a response that races back finds its waiter.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan rpcResult, 1)

	c.mu.Lock()
	if c.enc == nil {
		c.mu.Unlock()
		return nil, errNotConnected
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	err := c.enc.Encode(request{JSONRPC: "2.0", Method: method, Params: params, ID: id})
	c.mu.Unlock()

	if err != nil {
		c.cancel(id)
		return nil, fmt.Errorf("signalcli: write %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.cancel(id) // a late response is dropped
		return nil, ctx.Err()
	case res := <-ch:
		return res.result, res.err
	}
}

// notify sends a request without waiting for its response.
func (c *Client) notify(method string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enc == nil {
		return errNotConnected
	}
	c.nextID++
	return c.enc.Encode(request{JSONRPC: "2.0", Method: method, ID: c.nextID})
}

// complete delivers a result to the caller waiting on id, if any, and unregisters it.
// The waiter's channel is buffered, so the send never blocks.
func (c *Client) complete(id uint64, res rpcResult) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ok {
		ch <- res
	}
}

// cancel unregisters a pending call without delivering, so a caller that gave up
// leaves no waiter for a late response.
func (c *Client) cancel(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// teardown drops the connection's encoder and fails every in-flight call, so no caller
// waits forever across a reconnect.
func (c *Client) teardown() {
	c.mu.Lock()
	c.enc = nil
	pending := c.pending
	c.pending = make(map[uint64]chan rpcResult)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcResult{err: errNotConnected}
	}
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      uint64 `json:"id"`
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) err() error {
	if e == nil {
		return nil
	}
	return fmt.Errorf("signalcli: rpc error %d: %s", e.Code, e.Message)
}

// specFromParams extracts a text message from a "receive" notification, accepting the
// flat and the result-wrapped shapes different signal-cli versions emit. A non-text or
// empty message yields ok=false.
func specFromParams(raw json.RawMessage) (inbox.Spec, bool) {
	var p struct {
		Envelope *envelope `json:"envelope"`
		Result   *struct {
			Envelope *envelope `json:"envelope"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return inbox.Spec{}, false
	}
	env := p.Envelope
	if env == nil && p.Result != nil {
		env = p.Result.Envelope
	}
	if env == nil || env.DataMessage == nil || env.DataMessage.Message == "" {
		return inbox.Spec{}, false
	}
	conversation := env.Source
	if g := env.DataMessage.GroupInfo; g != nil && g.GroupID != "" {
		conversation = g.GroupID
	}
	return inbox.Spec{
		Conversation: conversation,
		Sender:       env.Source,
		Type:         "message",
		Content:      env.DataMessage.Message,
	}, true
}

type envelope struct {
	Source      string `json:"source"`
	DataMessage *struct {
		Message   string `json:"message"`
		GroupInfo *struct {
			GroupID string `json:"groupId"`
		} `json:"groupInfo"`
	} `json:"dataMessage"`
}

// A *Client is both an inbox Source and an inbox Sink.
var (
	_ inbox.Source = (*Client)(nil)
	_ inbox.Sink   = (*Client)(nil)
)
