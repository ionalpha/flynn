package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
)

// typeInto feeds each rune of s through the modal capture as a key press, the
// same path the app's event loop takes for a printable key.
func typeInto(handle func(input.Event) bool, s string) {
	for _, r := range s {
		handle(input.Key{Code: r})
	}
}

// awaitCapture polls until the host has installed a modal input capture and
// returns it, failing the test if none arrives.
func awaitCapture(t *testing.T, ui *fakeUI) func(input.Event) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if handle := ui.captureFn(); handle != nil {
			return handle
		}
		select {
		case <-deadline:
			t.Fatal("host never installed the approval capture")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// expiringContext is a context whose deadline the test passes by hand, so a
// grace-period expiry happens at a chosen point rather than on the wall clock.
type expiringContext struct {
	context.Context
	deadline time.Time
	done     chan struct{}
}

func newExpiringContext(deadline time.Time) *expiringContext {
	return &expiringContext{Context: context.Background(), deadline: deadline, done: make(chan struct{})}
}

func (c *expiringContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *expiringContext) Done() <-chan struct{}       { return c.done }
func (c *expiringContext) expire()                     { close(c.done) }

func (c *expiringContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

type approvalResult struct {
	dec mission.ApprovalDecision
	err error
}

// TestApprovalPromptAllowScopesGrant proves the interactive prompt renders the
// paused action and its countdown, and that typing a glob and pressing Enter
// returns an allow whose scope narrows the grant.
func TestApprovalPromptAllowScopesGrant(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	host.timing = clock.NewManual(time.Unix(1_700_000_000, 0))
	req := mission.ApprovalRequest{Action: "write_file", Host: "inst-7", Grace: 90 * time.Second}

	res := make(chan approvalResult, 1)
	go func() {
		dec, err := host.promptApproval(context.Background(), req)
		res <- approvalResult{dec, err}
	}()

	handle := awaitCapture(t, ui)
	// The overlay names the action, its host, and the grace countdown.
	overlay := strings.Join(ui.liveLines(80), "\n")
	for _, want := range []string{"approval required", "write_file", "inst-7", "auto-declines in 90s"} {
		if !strings.Contains(overlay, want) {
			t.Fatalf("approval overlay missing %q:\n%s", want, overlay)
		}
	}

	typeInto(handle, "src/**")
	handle(input.Key{Code: input.KeyEnter})

	got := <-res
	if got.err != nil {
		t.Fatalf("allow returned error: %v", got.err)
	}
	if !got.dec.Allow {
		t.Fatal("Enter did not allow the action")
	}
	if got.dec.Scope != "src/**" {
		t.Fatalf("allow scope = %q, want src/**", got.dec.Scope)
	}
	// The capture is cleared on the way out, returning input to the composer.
	if handle := ui.captureFn(); handle != nil {
		t.Fatal("capture was left installed after the prompt resolved")
	}
}

// TestApprovalPromptDenyWithFeedback proves Tab moves to the reason field and a
// typed reason plus Escape returns a deny carrying that feedback.
func TestApprovalPromptDenyWithFeedback(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	req := mission.ApprovalRequest{Action: "net_fetch", Host: "inst-7", Grace: time.Minute}

	res := make(chan approvalResult, 1)
	go func() {
		dec, err := host.promptApproval(context.Background(), req)
		res <- approvalResult{dec, err}
	}()

	handle := awaitCapture(t, ui)
	handle(input.Key{Code: input.KeyTab}) // move to the reason field
	typeInto(handle, "not this host")
	handle(input.Key{Code: input.KeyEsc})

	got := <-res
	if got.err != nil {
		t.Fatalf("deny returned error: %v", got.err)
	}
	if got.dec.Allow {
		t.Fatal("Escape allowed the action; it must deny")
	}
	if got.dec.Feedback != "not this host" {
		t.Fatalf("deny feedback = %q, want %q", got.dec.Feedback, "not this host")
	}
}

// TestApprovalPromptGraceExpiryDeclines proves the prompt resolves fail-closed
// when its context deadline (the grace period) expires with no decision: it
// returns the context error, which the waist treats as a decline.
func TestApprovalPromptGraceExpiryDeclines(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	req := mission.ApprovalRequest{Action: "write_file", Host: "inst-7", Grace: time.Minute}

	start := time.Unix(1_700_000_000, 0)
	host.timing = clock.NewManual(start)
	ctx := newExpiringContext(start.Add(req.Grace))
	res := make(chan approvalResult, 1)
	go func() {
		dec, err := host.promptApproval(ctx, req)
		res <- approvalResult{dec, err}
	}()

	// The deadline expires only once the prompt is open. A wall-clock timeout
	// here raced the goroutine: on a slow runner it fired before the capture
	// was installed, and the prompt opened and closed between two polls.
	awaitCapture(t, ui)
	ctx.expire()

	select {
	case got := <-res:
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("grace expiry returned %v, want context.DeadlineExceeded so the waist declines fail-closed", got.err)
		}
		if got.dec.Allow {
			t.Fatal("grace expiry produced an allow")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not resolve after the grace period expired")
	}
	if handle := ui.captureFn(); handle != nil {
		t.Fatal("capture was left installed after the grace period expired")
	}
}

// TestApprovalPromptResolvesOnce proves a second decision keystroke after the
// prompt has resolved is inert: the capture reports the event unhandled (so it
// falls through to the composer) and no second decision is sent.
func TestApprovalPromptResolvesOnce(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	req := mission.ApprovalRequest{Action: "write_file", Host: "inst-7", Grace: time.Minute}

	res := make(chan approvalResult, 1)
	go func() {
		dec, err := host.promptApproval(context.Background(), req)
		res <- approvalResult{dec, err}
	}()

	handle := awaitCapture(t, ui)
	handle(input.Key{Code: input.KeyEnter})
	<-res

	// The prompt is now inactive; a further event is not consumed by the modal.
	if host.approval.handle(input.Key{Code: input.KeyEnter}) {
		t.Fatal("an inactive approval prompt still consumed input")
	}
}

// TestApprovalPromptRenderEdit unit-tests the widget in isolation: focus moves
// with Tab, backspace deletes from the focused field, and the focused field
// shows a cursor while the idle one shows a placeholder.
func TestApprovalPromptRenderEdit(t *testing.T) {
	w := &approvalPrompt{th: theme.Default()}
	now := time.Unix(1_700_000_000, 0)
	done := w.open(mission.ApprovalRequest{Action: "write_file"}, now.Add(45*time.Second), clock.NewManual(now))

	// Scope field is focused first: type, then backspace once.
	typeInto(w.handle, "abx")
	if !w.handle(input.Key{Code: input.KeyBackspace}) {
		t.Fatal("backspace was not consumed by the active prompt")
	}
	rendered := strings.Join(w.Render(80), "\n")
	if !strings.Contains(rendered, "scope:  ab▏") {
		t.Fatalf("scope field did not render the edited value with a cursor:\n%s", rendered)
	}
	if !strings.Contains(rendered, "reason: -") {
		t.Fatalf("idle reason field did not render a placeholder:\n%s", rendered)
	}
	if !strings.Contains(rendered, "auto-declines in 45s") {
		t.Fatalf("countdown wrong:\n%s", rendered)
	}

	// Tab moves focus to the reason field; the cursor follows.
	w.handle(input.Key{Code: input.KeyTab})
	typeInto(w.handle, "why")
	rendered = strings.Join(w.Render(80), "\n")
	if !strings.Contains(rendered, "reason: why▏") {
		t.Fatalf("reason field did not take focus:\n%s", rendered)
	}

	w.handle(input.Key{Code: input.KeyEsc})
	select {
	case dec := <-done:
		if dec.Allow || dec.Feedback != "why" {
			t.Fatalf("decision = %+v, want deny with feedback why", dec)
		}
	default:
		t.Fatal("Escape did not resolve the prompt")
	}
	if w.Render(80) != nil {
		t.Fatal("a resolved prompt still rendered")
	}
}

// TestApprovalPrompterDelegates proves the mission.ApprovalPrompter adapter
// drives the host's prompt.
func TestApprovalPrompterDelegates(t *testing.T) {
	host, ui := newHostForTest(t, llmtest.NewScripted())
	var p mission.ApprovalPrompter = approvalPrompter{host: host}

	res := make(chan approvalResult, 1)
	go func() {
		dec, err := p.Prompt(context.Background(), mission.ApprovalRequest{Action: "write_file", Grace: time.Minute})
		res <- approvalResult{dec, err}
	}()
	handle := awaitCapture(t, ui)
	handle(input.Key{Code: input.KeyEnter})

	got := <-res
	if got.err != nil || !got.dec.Allow {
		t.Fatalf("prompter delegate = %+v/%v, want an allow", got.dec, got.err)
	}
}
