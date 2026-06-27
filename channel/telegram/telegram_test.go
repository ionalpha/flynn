package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/channel"
)

func TestNewRejectsEmptyToken(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") = nil error, want error")
	}
}

func TestReceiveDeliversTextMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/getUpdates") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// First poll (offset 0) returns one message; later polls block like a real
		// long poll until the client cancels, so the adapter does not busy-spin.
		if r.URL.Query().Get("offset") == "0" {
			writeOK(w, `[{"update_id":10,"message":{"text":"hello","chat":{"id":4242},"from":{"id":7,"username":"ada"}}}]`)
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c, err := New("tok", WithBaseURL(srv.URL), WithPollTimeout(time.Second))
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
	case msg := <-in:
		want := channel.Inbound{Chat: "4242", User: "ada", Text: "hello"}
		if msg != want {
			t.Fatalf("inbound = %+v, want %+v", msg, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the message")
	}
}

func TestReceiveSkipsNonTextAndAdvancesOffset(t *testing.T) {
	var sawSecondPoll bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("offset") {
		case "0":
			// A non-text update (e.g. a photo) carries no Text and must be skipped,
			// but its id must still advance the offset so it is not re-fetched.
			writeOK(w, `[{"update_id":99,"message":{"chat":{"id":1}}}]`)
		case "100":
			sawSecondPoll = true
			writeOK(w, `[{"update_id":100,"message":{"text":"hi","chat":{"id":1}}}]`)
		default:
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	c, _ := New("tok", WithBaseURL(srv.URL), WithPollTimeout(time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in, _ := c.Receive(ctx)

	select {
	case msg := <-in:
		if msg.Text != "hi" {
			t.Fatalf("text = %q, want hi", msg.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	if !sawSecondPoll {
		t.Fatal("offset did not advance past the skipped non-text update")
	}
}

func TestSendPostsMessage(t *testing.T) {
	type sent struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	got := make(chan sent, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/sendMessage") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var s sent
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			t.Errorf("decode body: %v", err)
		}
		got <- s
		writeOK(w, `{"message_id":1}`)
	}))
	defer srv.Close()

	c, _ := New("tok", WithBaseURL(srv.URL))
	if err := c.Send(context.Background(), channel.Outbound{Chat: "4242", Text: "yo"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	s := <-got
	if s.ChatID != "4242" || s.Text != "yo" {
		t.Fatalf("server got %+v, want {4242 yo}", s)
	}
}

func TestSendSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"description":"chat not found"}`))
	}))
	defer srv.Close()

	c, _ := New("tok", WithBaseURL(srv.URL))
	err := c.Send(context.Background(), channel.Outbound{Chat: "x", Text: "y"})
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("Send err = %v, want it to carry the API description", err)
	}
}

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("", 10); got != nil {
		t.Errorf("splitMessage(\"\") = %v, want nil", got)
	}
	if got := splitMessage("hi", 10); len(got) != 1 || got[0] != "hi" {
		t.Errorf("short = %v, want [hi]", got)
	}
	long := strings.Repeat("a", 9001)
	parts := splitMessage(long, 4000)
	if len(parts) != 3 {
		t.Fatalf("chunks = %d, want 3", len(parts))
	}
	if strings.Join(parts, "") != long {
		t.Error("rejoined chunks do not equal the original")
	}
	for i, p := range parts {
		if len([]rune(p)) > 4000 {
			t.Errorf("chunk %d exceeds the limit", i)
		}
	}
}

func TestSendChunksLongReply(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		writeOK(w, `{"message_id":1}`)
	}))
	defer srv.Close()

	c, _ := New("tok", WithBaseURL(srv.URL))
	if err := c.Send(context.Background(), channel.Outbound{Chat: "1", Text: strings.Repeat("x", 9001)}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("sendMessage calls = %d, want 3", calls)
	}
}

func writeOK(w http.ResponseWriter, result string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"result":` + result + `}`))
}
