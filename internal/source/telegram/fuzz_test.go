package telegram

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

// bodyTransport answers every request with a fixed status and body, so a fuzz
// input drives the envelope decode without a socket.
type bodyTransport struct {
	status int
	body   []byte
}

func (t bodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: t.status,
		Body:       io.NopCloser(bytes.NewReader(t.body)),
		Header:     make(http.Header),
	}, nil
}

// FuzzGetUpdates drives the Telegram getUpdates decode: the two-stage unmarshal of
// the API envelope (ok/result/description) and then the update list. Bot API JSON
// is untrusted, so the bar is that no body panics: a malformed envelope or result
// surfaces as an error, and the update->sender projection over any decoded update
// never faults on a missing message or author.
func FuzzGetUpdates(f *testing.F) {
	seeds := []string{
		`{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":9},"from":{"id":3,"username":"u"},"text":"hi"}}]}`,
		`{"ok":true,"result":[{"update_id":2,"message":{"chat":{"id":9},"text":"no from"}}]}`,
		`{"ok":true,"result":[{"update_id":3}]}`, // update with no message
		`{"ok":true,"result":[]}`,
		`{"ok":false,"description":"bad token"}`,
		`{"ok":true,"result":"not an array"}`, // result is the wrong shape
		`{"ok":true}`,
		`{}`,
		`null`,
		`not json`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		b, err := New("token", WithHTTPClient(&http.Client{Transport: bodyTransport{status: http.StatusOK, body: body}}))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		updates, err := b.getUpdates(context.Background(), 0)
		if err != nil {
			return // a typed error is the correct outcome for a malformed body
		}
		// Exercise the projection that Receive runs over each decoded update.
		for _, u := range updates {
			if u.Message != nil {
				_ = u.Message.sender()
			}
		}
	})
}
