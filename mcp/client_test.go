package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// clientPipes wires a Client to a scripted peer over two in-memory pipes, the transport
// the sandbox launch would otherwise supply from a subprocess's stdio. The peer runs
// handle for every request line and writes back whatever it returns; returning reply
// false models a peer that stays silent on that request (used to prove the anti-hang
// deadline). Close tears both pipes down, modelling a dead peer.
type clientPipes struct {
	client *Client
	// stop ends the peer goroutine and closes the pipes.
	stop func()
}

// startPeer builds a Client talking to a peer driven by handle. handle receives the
// method, the raw id, and the raw params of each request and returns the raw reply line
// to send (without a trailing newline) and whether to send it at all.
func startPeer(t *testing.T, handle func(method string, id, params json.RawMessage) (reply []byte, send bool)) *clientPipes {
	t.Helper()
	// client writes requests to clientW, the peer reads them from peerR.
	peerR, clientW := io.Pipe()
	// peer writes replies to peerW, the client reads them from clientR.
	clientR, peerW := io.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(peerR)
		sc.Buffer(make([]byte, 0, 64<<10), maxClientMessageBytes)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var req clientRequest
			if err := json.Unmarshal(line, &req); err != nil {
				continue
			}
			if req.ID == nil {
				continue // a notification: nothing to answer
			}
			rawID, _ := json.Marshal(*req.ID)
			reply, send := handle(req.Method, rawID, req.Params)
			if !send {
				continue
			}
			if _, err := peerW.Write(append(reply, '\n')); err != nil {
				return
			}
		}
	}()

	c := NewClient(clientR, clientW)
	stop := func() {
		_ = clientW.Close()
		_ = peerW.Close()
		_ = c.Close()
		<-done
	}
	t.Cleanup(stop)
	return &clientPipes{client: c, stop: stop}
}

// okReply builds a well-formed success reply echoing id with the given result value.
func okReply(t *testing.T, id json.RawMessage, result any) []byte {
	t.Helper()
	res, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	b.Write(id)
	b.WriteString(`,"result":`)
	b.Write(res)
	b.WriteByte('}')
	return b.Bytes()
}

// scriptedServer answers initialize/list/call as a healthy server would, so the happy
// path exercises the real handshake and framing.
func scriptedServer(t *testing.T) func(method string, id, params json.RawMessage) (reply []byte, send bool) {
	return func(method string, id, params json.RawMessage) ([]byte, bool) {
		switch method {
		case "initialize":
			return okReply(t, id, initializeResult{
				ProtocolVersion: protocolVersion,
				ServerInfo:      Info{Name: "stub", Version: "1.0"},
			}), true
		case "tools/list":
			return okReply(t, id, toolsListResult{Tools: []toolDef{
				{Name: "echo", Description: "echoes", InputSchema: json.RawMessage(`{"type":"object"}`)},
			}}), true
		case "tools/call":
			var cp callParams
			_ = json.Unmarshal(params, &cp)
			return okReply(t, id, callResult{Content: textContent("called:" + cp.Name)}), true
		default:
			return okReply(t, id, struct{}{}), true
		}
	}
}

func TestClientHandshakeAndCall(t *testing.T) {
	p := startPeer(t, scriptedServer(t))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	info, err := p.client.Initialize(ctx)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if info.Name != "stub" {
		t.Fatalf("server info name = %q, want stub", info.Name)
	}

	tools, err := p.client.ListTools(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want one named echo", tools)
	}

	res, err := p.client.CallTool(ctx, "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Text != "called:echo" || res.IsError {
		t.Fatalf("call result = %+v, want called:echo", res)
	}
}

// TestClientCallTimeoutDoesNotHang proves the anti-hang control: a peer that never
// answers a request must not wedge the caller; the request's context deadline releases
// it with a timeout error.
func TestClientCallTimeoutDoesNotHang(t *testing.T) {
	p := startPeer(t, func(method string, id, params json.RawMessage) ([]byte, bool) {
		if method == "tools/call" {
			return nil, false // stay silent on the call
		}
		return okReply(t, id, initializeResult{ProtocolVersion: protocolVersion}), true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := p.client.CallTool(ctx, "hang", nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CallTool did not return after its context deadline: it hung")
	}
}

// TestClientOversizeReplyFailsClosed proves the bounded-read control: a reply larger than
// the message limit is a transport error that fails the pending call rather than being
// buffered without bound.
func TestClientOversizeReplyFailsClosed(t *testing.T) {
	p := startPeer(t, func(method string, id, params json.RawMessage) ([]byte, bool) {
		if method == "tools/call" {
			// A single line larger than the cap, no newline until the end, forces the
			// bounded scanner to give up with ErrTooLong.
			huge := bytes.Repeat([]byte("A"), maxClientMessageBytes+1024)
			return huge, true
		}
		return okReply(t, id, initializeResult{ProtocolVersion: protocolVersion}), true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := p.client.CallTool(ctx, "big", nil)
	if err == nil {
		t.Fatal("expected an error for an oversize reply, got nil")
	}
}

// TestClientIgnoresMismatchedID proves the id-matching control: a reply carrying an id
// the client never sent is dropped, never delivered to a waiting caller. The pending call
// then correctly times out rather than resolving on a foreign reply.
func TestClientIgnoresMismatchedID(t *testing.T) {
	p := startPeer(t, func(method string, id, params json.RawMessage) ([]byte, bool) {
		if method == "tools/call" {
			// Reply under a bogus id the client never issued.
			return okReply(t, json.RawMessage(`999999`), callResult{Content: textContent("foreign")}), true
		}
		return okReply(t, id, initializeResult{ProtocolVersion: protocolVersion}), true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res, err := p.client.CallTool(ctx, "x", nil)
	if err == nil {
		t.Fatalf("expected timeout (foreign id ignored), got result %+v", res)
	}
}

// TestClientDropsDuplicateReply proves a peer that answers the same id twice does not
// panic the client (the second delivery finds no waiter) and the first answer stands. The
// peer writes the duplicate line itself so the reply genuinely repeats on the wire.
func TestClientDropsDuplicateReply(t *testing.T) {
	peerR, clientW := io.Pipe()
	clientR, peerW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(peerR)
		sc.Buffer(make([]byte, 0, 64<<10), maxClientMessageBytes)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var req clientRequest
			if err := json.Unmarshal(line, &req); err != nil || req.ID == nil {
				continue
			}
			rawID, _ := json.Marshal(*req.ID)
			var reply []byte
			if req.Method == "tools/call" {
				reply = okReply(t, rawID, callResult{Content: textContent("first")})
			} else {
				reply = okReply(t, rawID, initializeResult{ProtocolVersion: protocolVersion})
			}
			// Send the reply, then send it a second time: the duplicate must be dropped.
			_, _ = peerW.Write(append(reply, '\n'))
			_, _ = peerW.Write(append(reply, '\n'))
		}
	}()
	c := NewClient(clientR, clientW)
	t.Cleanup(func() { _ = clientW.Close(); _ = peerW.Close(); _ = c.Close(); <-done })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, "dup", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Text != "first" {
		t.Fatalf("result = %q, want first", res.Text)
	}
	// A second call must still work, proving the duplicate did not wedge the read loop.
	if _, err := c.CallTool(ctx, "again", nil); err != nil {
		t.Fatalf("second call after duplicate: %v", err)
	}
}

// TestClientDeadPeerFailsInFlight proves that tearing down the transport fails an
// in-flight call closed rather than leaving it hung.
func TestClientDeadPeerFailsInFlight(t *testing.T) {
	p := startPeer(t, func(method string, id, params json.RawMessage) ([]byte, bool) {
		if method == "tools/call" {
			return nil, false // never answer; the test kills the transport instead
		}
		return okReply(t, id, initializeResult{ProtocolVersion: protocolVersion}), true
	})

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := p.client.CallTool(ctx, "orphan", nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	p.stop() // kill the transport under the in-flight call

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a closed-transport error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight call did not fail after the peer died")
	}
}

// TestClientConcurrentCallsMatched proves replies are routed to the right caller under
// concurrency: many calls in flight at once each get their own answer, keyed by id, even
// when the peer answers out of order.
func TestClientConcurrentCallsMatched(t *testing.T) {
	p := startPeer(t, func(method string, id, params json.RawMessage) ([]byte, bool) {
		if method == "tools/call" {
			var cp callParams
			_ = json.Unmarshal(params, &cp)
			return okReply(t, id, callResult{Content: textContent("r:" + cp.Name)}), true
		}
		return okReply(t, id, initializeResult{ProtocolVersion: protocolVersion}), true
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "t" + strings.Repeat("x", i)
			res, err := p.client.CallTool(ctx, name, nil)
			if err != nil {
				errs <- err
				return
			}
			if res.Text != "r:"+name {
				errs <- fmt.Errorf("call %d got %q", i, res.Text)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestClientCallAfterCloseFailsClosed proves a call on a closed client returns an error
// rather than blocking or panicking.
func TestClientCallAfterCloseFailsClosed(t *testing.T) {
	p := startPeer(t, scriptedServer(t))
	_ = p.client.Close()
	ctx := context.Background()
	if _, err := p.client.CallTool(ctx, "x", nil); err == nil {
		t.Fatal("expected an error calling a closed client")
	}
	if _, err := p.client.Initialize(ctx); err == nil {
		t.Fatal("expected an error initializing a closed client")
	}
}
