package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	case spec := <-in:
		if spec.Conversation != "4242" || spec.Sender != "ada" || spec.Type != "message" || spec.Content != "hello" {
			t.Fatalf("spec = %+v, want {Conversation:4242 Sender:ada Type:message Content:hello}", spec)
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
	case spec := <-in:
		if spec.Content != "hi" {
			t.Fatalf("content = %q, want hi", spec.Content)
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
	if err := c.Send(context.Background(), "4242", "yo"); err != nil {
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
	err := c.Send(context.Background(), "x", "y")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("Send err = %v, want it to carry the API description", err)
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
	if err := c.Send(context.Background(), "1", strings.Repeat("x", 9001)); err != nil {
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
