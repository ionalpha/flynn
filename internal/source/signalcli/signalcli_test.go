package signalcli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestNewRejectsEmptyAddr(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") = nil error, want error")
	}
}

// serve starts a fake signal-cli daemon on a loopback port and runs handle on the
// first connection.
func serve(t *testing.T, handle func(conn net.Conn)) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		handle(conn)
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestReceiveDeliversIncomingMessage(t *testing.T) {
	addr, closeFn := serve(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+123","dataMessage":{"message":"hi"}}}}` + "\n"))
		_, _ = io.Copy(io.Discard, conn) // keep open until the client closes
	})
	defer closeFn()

	c, err := New(addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, err := c.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case spec := <-in:
		if spec.Conversation != "+123" || spec.Sender != "+123" || spec.Type != "message" || spec.Content != "hi" {
			t.Fatalf("spec = %+v, want {Conversation:+123 Sender:+123 Type:message Content:hi}", spec)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the message")
	}
}

func TestReceiveDeliversGroupMessage(t *testing.T) {
	addr, closeFn := serve(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+1","dataMessage":{"message":"yo","groupInfo":{"groupId":"GRP=="}}}}}` + "\n"))
		_, _ = io.Copy(io.Discard, conn)
	})
	defer closeFn()

	c, _ := New(addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, _ := c.Receive(ctx)
	select {
	case spec := <-in:
		if spec.Conversation != "GRP==" || spec.Sender != "+1" || spec.Content != "yo" {
			t.Fatalf("group spec = %+v, want conversation=GRP== sender=+1 content=yo", spec)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func TestSendCorrelatesResponse(t *testing.T) {
	gotReq := make(chan map[string]any, 1)
	addr, closeFn := serve(t, func(conn net.Conn) {
		defer func() { _ = conn.Close() }()
		// Prove the connection is up so the client can send.
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"source":"+1","dataMessage":{"message":"ping"}}}}` + "\n"))
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var m struct {
					ID     *uint64        `json:"id"`
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
				}
				if json.Unmarshal(line, &m) == nil && m.Method == "send" && m.ID != nil {
					gotReq <- m.Params
					_, _ = fmt.Fprintf(conn, `{"jsonrpc":"2.0","id":%d,"result":{"timestamp":1}}`+"\n", *m.ID)
				}
			}
			if err != nil {
				return
			}
		}
	})
	defer closeFn()

	c, _ := New(addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, _ := c.Receive(ctx)
	<-in // connection established

	if err := c.Send(ctx, "+999", "yo"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case req := <-gotReq:
		if req["message"] != "yo" {
			t.Errorf("message = %v, want yo", req["message"])
		}
		rcpt, ok := req["recipient"].([]any)
		if !ok || len(rcpt) != 1 || rcpt[0] != "+999" {
			t.Errorf("recipient = %v, want [+999]", req["recipient"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the send")
	}
}

func TestSendWithoutConnectionErrors(t *testing.T) {
	c, _ := New("127.0.0.1:1")
	if err := c.Send(context.Background(), "+1", "x"); err == nil {
		t.Fatal("Send with no connection = nil, want error")
	}
}

// TestSendParamsProperty is the rigor property: every conversation routes to
// exactly one of a direct recipient or a group id, a +number is always a recipient,
// and the message is carried verbatim.
func TestSendParamsProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		conv := rapid.String().Draw(rt, "conversation")
		text := rapid.String().Draw(rt, "text")
		p := sendParams(conv, text, "")

		if p["message"] != text {
			rt.Fatalf("message = %v, want %q", p["message"], text)
		}
		_, hasRecipient := p["recipient"]
		_, hasGroup := p["groupId"]
		if hasRecipient == hasGroup {
			rt.Fatalf("want exactly one of recipient/groupId for %q: recipient=%v group=%v", conv, hasRecipient, hasGroup)
		}
		if strings.HasPrefix(conv, "+") && !hasRecipient {
			rt.Fatalf("a +number must route to a recipient: %q", conv)
		}
		if !strings.HasPrefix(conv, "+") && !hasGroup {
			rt.Fatalf("a non-+ id must route to a group: %q", conv)
		}
	})
}
