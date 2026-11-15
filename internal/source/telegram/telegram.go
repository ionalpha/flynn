// Package telegram adapts the Telegram Bot API to the inbox Source and Sink ports. It
// long-polls getUpdates and replies with sendMessage, using only the standard library.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/inbox"
)

// name is the source's identity, matching the Sink so replies route back.
const name = "telegram"

const defaultAPIBase = "https://api.telegram.org"

// defaultPollTimeout is how long getUpdates asks the server to hold a request open, so
// an idle bot makes one blocked request instead of a busy loop.
const defaultPollTimeout = 30 * time.Second

// retryBackoff paces reconnection after a transient getUpdates failure.
const retryBackoff = 2 * time.Second

// maxMessageLen keeps each sendMessage under Telegram's 4096-character limit. A longer
// reply is split across messages.
const maxMessageLen = 4000

// Bot is a Telegram bot that is both an inbox Source and an inbox Sink.
type Bot struct {
	token   string
	baseURL string
	http    *http.Client
	poll    time.Duration
	clk     clock.Timing
}

// Option configures a Bot.
type Option func(*Bot)

// WithHTTPClient sets the HTTP client for API calls. The default client has no timeout
// because getUpdates long-polls; each request gets its own deadline instead.
func WithHTTPClient(c *http.Client) Option {
	return func(b *Bot) {
		if c != nil {
			b.http = c
		}
	}
}

// WithBaseURL overrides the API root (for tests).
func WithBaseURL(u string) Option {
	return func(b *Bot) {
		if u != "" {
			b.baseURL = u
		}
	}
}

// WithPollTimeout overrides how long each getUpdates call waits for a message.
func WithPollTimeout(d time.Duration) Option {
	return func(b *Bot) {
		if d > 0 {
			b.poll = d
		}
	}
}

// WithClock sets the clock that paces reconnect backoff, so a test drives retry timing
// with a Manual clock instead of sleeping.
func WithClock(clk clock.Timing) Option {
	return func(b *Bot) {
		if clk != nil {
			b.clk = clk
		}
	}
}

// New builds a Telegram bot for token. The token is required and never logged or
// wrapped into an error.
func New(token string, opts ...Option) (*Bot, error) {
	if token == "" {
		return nil, errors.New("telegram: empty bot token")
	}
	b := &Bot{
		token:   token,
		baseURL: defaultAPIBase,
		http:    &http.Client{},
		poll:    defaultPollTimeout,
		clk:     clock.System{},
	}
	for _, o := range opts {
		o(b)
	}
	return b, nil
}

// Name identifies the source and its paired sink.
func (b *Bot) Name() string { return name }

// Receive streams each text message as an inbox.Spec until ctx is cancelled, then
// closes the channel. A failed poll is retried after a backoff instead of ending the
// stream. The Spec's Source is left for the ingester to stamp.
func (b *Bot) Receive(ctx context.Context) (<-chan inbox.Spec, error) {
	out := make(chan inbox.Spec)
	go func() {
		defer close(out)
		var offset int64
		inbox.ReceiveLoop(ctx, retryBackoff, b.clk, func(ctx context.Context) error {
			updates, err := b.getUpdates(ctx, offset)
			if err != nil {
				return err // ReceiveLoop backs off before the next poll
			}
			for _, u := range updates {
				offset = u.UpdateID + 1 // acknowledge: never re-fetch this update
				msg := u.Message
				if msg == nil || msg.Text == "" {
					continue // non-text updates are out of scope for this adapter
				}
				spec := inbox.Spec{
					Conversation: strconv.FormatInt(msg.Chat.ID, 10),
					Sender:       msg.sender(),
					Type:         "message",
					Content:      msg.Text,
				}
				select {
				case out <- spec:
				case <-ctx.Done():
					return nil // cancelled; ReceiveLoop exits on the next ctx check
				}
			}
			return nil // successful poll; ReceiveLoop long-polls again immediately
		})
	}()
	return out, nil
}

// Send delivers a reply with sendMessage, splitting an over-long one so it arrives in
// full.
func (b *Bot) Send(ctx context.Context, conversation, text string) error {
	for _, part := range splitMessage(text, maxMessageLen) {
		body := map[string]string{"chat_id": conversation, "text": part}
		var ignored json.RawMessage
		if err := b.post(ctx, "sendMessage", body, &ignored); err != nil {
			return err
		}
	}
	return nil
}

// splitMessage breaks s into chunks of at most limit runes, so a multi-byte character
// is never cut. An empty string yields no chunks.
func splitMessage(s string, limit int) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return []string{s}
	}
	var parts []string
	for len(runes) > 0 {
		n := min(limit, len(runes))
		parts = append(parts, string(runes[:n]))
		runes = runes[n:]
	}
	return parts
}

// getUpdates fetches updates at or after offset. The request deadline sits above the
// poll timeout, so the client never cancels a poll the server is still honouring.
func (b *Bot) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	pollSecs := int(b.poll / time.Second)
	q := url.Values{}
	q.Set("timeout", strconv.Itoa(pollSecs))
	q.Set("offset", strconv.FormatInt(offset, 10))
	q.Set("allowed_updates", `["message"]`)

	reqCtx, cancel := context.WithTimeout(ctx, b.poll+10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, b.method("getUpdates")+"?"+q.Encode(), http.NoBody)
	if err != nil {
		return nil, err
	}
	var updates []update
	if err := b.do(req, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (b *Bot) post(ctx context.Context, method string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.method(method), bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return b.do(req, out)
}

// do executes req and decodes the Telegram envelope. A transport failure, a non-200
// status and ok=false are errors, and none of them includes the bot token.
func (b *Bot) do(req *http.Request, out any) error {
	resp, err := b.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", req.Method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("telegram: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: unexpected status %d", resp.StatusCode)
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("telegram: decode response: %w", err)
	}
	if !env.OK {
		return fmt.Errorf("telegram: api error: %s", env.Description)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram: decode result: %w", err)
	}
	return nil
}

// method builds the API URL for a bot method. The token is in the path, as the Bot API
// requires, so the URL must be kept out of logs.
func (b *Bot) method(m string) string {
	return b.baseURL + "/bot" + b.token + "/" + m
}

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	Chat chat   `json:"chat"`
	From *user  `json:"from"`
	Text string `json:"text"`
}

// sender returns the author's numeric user id, empty when the message names none. An
// allow-list needs the id because it never changes, while a username can be renamed
// and claimed by someone else.
func (m *message) sender() string {
	if m.From == nil {
		return ""
	}
	return strconv.FormatInt(m.From.ID, 10)
}

type chat struct {
	ID int64 `json:"id"`
}

type user struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// A *Bot is both an inbox Source and an inbox Sink.
var (
	_ inbox.Source = (*Bot)(nil)
	_ inbox.Sink   = (*Bot)(nil)
)
