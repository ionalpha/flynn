// Package telegram adapts the Telegram Bot API to the inbox Source and Sink ports.
// It receives messages by long-polling getUpdates and replies with sendMessage,
// using only the standard library so it ships in the single static binary with no
// extra dependencies.
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

	"github.com/ionalpha/flynn/inbox"
)

// name is the source's stable identity, matching the Sink so replies route back.
const name = "telegram"

// defaultAPIBase is the Telegram Bot API root. Override it in tests with
// WithBaseURL.
const defaultAPIBase = "https://api.telegram.org"

// defaultPollTimeout is how long getUpdates is asked to hold a request open
// waiting for a message (server-side long poll), so an idle bot makes one blocked
// request rather than a busy loop.
const defaultPollTimeout = 30 * time.Second

// retryBackoff paces reconnection after a transient getUpdates failure.
const retryBackoff = 2 * time.Second

// maxMessageLen keeps each sendMessage under Telegram's per-message limit. A longer
// reply is split across messages rather than rejected.
const maxMessageLen = 4000

// Bot is a Telegram bot adapted to the inbox ports: it is both a Source (inbound
// messages) and a Sink (outbound replies). Construct it with New.
type Bot struct {
	token   string
	baseURL string
	http    *http.Client
	poll    time.Duration
}

// Option configures a Bot.
type Option func(*Bot)

// WithHTTPClient sets the HTTP client used for API calls. The default client has no
// timeout because getUpdates long-polls; per-request deadlines are applied
// internally instead.
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

// New builds a Telegram bot for the given token. The token is required and is never
// logged or wrapped into an error.
func New(token string, opts ...Option) (*Bot, error) {
	if token == "" {
		return nil, errors.New("telegram: empty bot token")
	}
	b := &Bot{
		token:   token,
		baseURL: defaultAPIBase,
		http:    &http.Client{},
		poll:    defaultPollTimeout,
	}
	for _, o := range opts {
		o(b)
	}
	return b, nil
}

// Name identifies the source and its paired sink.
func (b *Bot) Name() string { return name }

// Receive long-polls getUpdates and streams each text message as an inbox.Spec
// until ctx is cancelled, then closes the returned channel. Transient request
// failures are retried after a short backoff rather than ending the stream. The
// Spec's Source is left for the ingester to stamp.
func (b *Bot) Receive(ctx context.Context) (<-chan inbox.Spec, error) {
	out := make(chan inbox.Spec)
	go func() {
		defer close(out)
		var offset int64
		for {
			if ctx.Err() != nil {
				return
			}
			updates, err := b.getUpdates(ctx, offset)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(retryBackoff):
				}
				continue
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
					return
				}
			}
		}
	}()
	return out, nil
}

// Send delivers a reply to a conversation with sendMessage, splitting an over-long
// reply into several messages so a large answer is delivered in full.
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

// splitMessage breaks s into chunks of at most limit runes, splitting on rune
// boundaries so a multi-byte character is never cut. An empty string yields no
// chunks, so nothing is sent.
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

// getUpdates fetches the next batch of updates at or after offset, asking the
// server to hold the request open for the poll timeout. The request deadline sits
// above the poll timeout so the client never cancels a poll the server is honoring.
func (b *Bot) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	pollSecs := int(b.poll / time.Second)
	q := url.Values{}
	q.Set("timeout", strconv.Itoa(pollSecs))
	q.Set("offset", strconv.FormatInt(offset, 10))
	q.Set("allowed_updates", `["message"]`)

	reqCtx, cancel := context.WithTimeout(ctx, b.poll+10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, b.method("getUpdates")+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var updates []update
	if err := b.do(req, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// post calls an API method with a JSON body and decodes result into out.
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

// do executes req and decodes the Telegram envelope, returning an error when the
// transport fails, the HTTP status is not 200, or the API reports ok=false. The bot
// token is never included in an error.
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

// method builds the API URL for a bot method. The token lives in the path, as the
// Bot API requires; callers must keep it out of logs.
func (b *Bot) method(m string) string {
	return b.baseURL + "/bot" + b.token + "/" + m
}

// update is one Telegram update; only text messages are consumed.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	Chat chat   `json:"chat"`
	From *user  `json:"from"`
	Text string `json:"text"`
}

// sender returns a stable identifier for the author: the username when set,
// otherwise the numeric id, for routing and audit.
func (m *message) sender() string {
	if m.From == nil {
		return ""
	}
	if m.From.Username != "" {
		return m.From.Username
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

// guards: a *Bot is both an inbox Source and an inbox Sink.
var (
	_ inbox.Source = (*Bot)(nil)
	_ inbox.Sink   = (*Bot)(nil)
)
