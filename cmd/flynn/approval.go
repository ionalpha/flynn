package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
	"github.com/ionalpha/flynn/mission"
)

// defaultPromptGrace bounds the approval prompt's countdown when the request
// carries no grace of its own and its context has no deadline. It mirrors the
// runtime's own fail-closed backstop, so a prompt is never left waiting forever
// on an unattended terminal. In practice the waist always wraps the prompt
// context with the grace deadline, so this is only the defensive fallback.
const defaultPromptGrace = 2 * time.Minute

// approvalPrompter is the shell's mission.ApprovalPrompter: it hands a paused
// privileged action to the interactive prompt and blocks the run goroutine on
// the operator's decision. The waist calls Prompt when a governed action needs
// approval; the decision it returns is minted into a single-use approval and
// the action retried, or the action is refused with the operator's feedback.
type approvalPrompter struct{ host *sessionHost }

// Prompt implements mission.ApprovalPrompter by driving the host's live prompt.
// It runs on the run goroutine and blocks until the operator decides or the
// grace period (the prompt context's deadline) expires, which resolves to a
// fail-closed decline.
func (p approvalPrompter) Prompt(ctx context.Context, req mission.ApprovalRequest) (mission.ApprovalDecision, error) {
	return p.host.promptApproval(ctx, req)
}

var _ mission.ApprovalPrompter = approvalPrompter{}

// approvalPrompt is the shell's interactive approval overlay: a modal prompt
// rendered in the live region when a governed action pauses for a human
// decision. It shows the action and the host it would run on, a countdown to
// the grace-period expiry, and two editable fields: an exact glob that narrows
// an allow so it does not over-grant, and a short reason fed back to the run on
// a deny. Enter allows, Escape denies, Tab moves between the fields. It renders
// nothing while inactive, so it can sit permanently at the top of the live
// stack like the governance panel, and its own mutex guards the snapshot so the
// paint goroutine's Render and the run goroutine's open/clear never race.
type approvalPrompt struct {
	th *theme.Theme

	mu       sync.Mutex
	active   bool
	req      mission.ApprovalRequest
	scope    string
	feedback string
	focus    int // 0 = scope field, 1 = feedback field
	deadline time.Time
	clk      clock.Clock
	done     chan mission.ApprovalDecision
}

// open arms the prompt for one request: it resets the fields, records the
// grace-period deadline the countdown renders against, and returns the channel
// the operator's decision arrives on. The channel is buffered so the event-loop
// goroutine's resolve never blocks under the lock. It is called on the run
// goroutine; the paint goroutine sees the prompt go active on its next frame.
func (w *approvalPrompt) open(req mission.ApprovalRequest, deadline time.Time, clk clock.Clock) <-chan mission.ApprovalDecision {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.req = req
	w.scope = ""
	w.feedback = ""
	w.focus = 0
	w.deadline = deadline
	w.clk = clk
	w.done = make(chan mission.ApprovalDecision, 1)
	w.active = true
	return w.done
}

// clear disarms the prompt so it renders nothing again. It is idempotent: a
// prompt already resolved by a keystroke is cleared once more by the run
// goroutine on its way out, which is harmless.
func (w *approvalPrompt) clear() {
	w.mu.Lock()
	w.active = false
	w.mu.Unlock()
}

// handle is the app's modal capture while the prompt is up: it owns every event,
// mapping Enter to allow (with the scope field as the grant's exact glob),
// Escape to deny (with the feedback field as the reason), Tab to move focus, and
// printable keys and pastes to the focused field. It returns false only when the
// prompt is inactive, so a stray event after the prompt clears falls through to
// the composer rather than being swallowed.
func (w *approvalPrompt) handle(ev input.Event) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return false
	}
	switch e := ev.(type) {
	case input.Key:
		switch e.Code {
		case input.KeyEnter:
			w.resolveLocked(mission.ApprovalDecision{Allow: true, Scope: strings.TrimSpace(w.scope)})
		case input.KeyEsc:
			w.resolveLocked(mission.ApprovalDecision{Allow: false, Feedback: strings.TrimSpace(w.feedback)})
		case input.KeyTab:
			w.focus ^= 1
		case input.KeyBackspace:
			w.backspaceLocked()
		default:
			if t := e.Text(); t != "" {
				w.appendLocked(t)
			}
		}
	case input.Paste:
		w.appendLocked(e.Text)
	default:
		// A focus change or an unknown sequence is swallowed while the modal is
		// up, so nothing leaks to the composer behind it.
	}
	return true
}

// field returns a pointer to the currently focused editable field.
func (w *approvalPrompt) field() *string {
	if w.focus == 1 {
		return &w.feedback
	}
	return &w.scope
}

// appendLocked adds text to the focused field, stripping control characters and
// newlines so a pasted block stays a single-line glob or reason.
func (w *approvalPrompt) appendLocked(text string) {
	var b strings.Builder
	for _, r := range text {
		if r >= ' ' {
			b.WriteRune(r)
		}
	}
	f := w.field()
	*f += b.String()
}

// backspaceLocked deletes the last rune of the focused field.
func (w *approvalPrompt) backspaceLocked() {
	f := w.field()
	if *f == "" {
		return
	}
	r := []rune(*f)
	*f = string(r[:len(r)-1])
}

// resolveLocked sends one decision and disarms the prompt. It is guarded so a
// second Enter/Escape after the first cannot send twice on a channel the run
// goroutine has already read and abandoned.
func (w *approvalPrompt) resolveLocked(dec mission.ApprovalDecision) {
	if !w.active {
		return
	}
	w.active = false
	w.done <- dec
}

// remainingLocked is the whole seconds left before the grace period expires,
// never negative. It is what the countdown row shows.
func (w *approvalPrompt) remainingLocked() int {
	var c clock.Clock = clock.System{}
	if w.clk != nil {
		c = w.clk
	}
	d := w.deadline.Sub(c.Now())
	if d <= 0 {
		return 0
	}
	// Round up so a sub-second remainder still reads as a second left, not zero.
	return int((d + time.Second - 1) / time.Second)
}

// Render draws the approval prompt, or nothing when it is inactive. Every row is
// truncated to width so the overlay never overflows, and the focused field
// carries a cursor so it is clear which one a keystroke edits.
func (w *approvalPrompt) Render(width int) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return nil
	}

	var rows []string
	add := func(role theme.Role, text string) {
		rows = append(rows, w.th.Render(role, screen.Truncate(text, width)))
	}

	add(theme.Heading, "approval required  (enter allow · esc deny · tab switches field)")
	action := strings.TrimSpace(w.req.Action)
	if action == "" {
		action = "action"
	}
	line := "  action: " + action
	if host := strings.TrimSpace(w.req.Host); host != "" {
		line += "   host: " + host
	}
	add(theme.Rejected, line)
	add(w.fieldRole(0), "  scope:  "+w.fieldText(0)+"   (allow narrows the grant to this glob)")
	add(w.fieldRole(1), "  reason: "+w.fieldText(1)+"   (deny sends this back to the run)")
	add(theme.StatusBusy, fmt.Sprintf("  auto-declines in %ds", w.remainingLocked()))
	return rows
}

// fieldRole styles the focused field brighter than the idle one, so the target
// of the next keystroke reads at a glance.
func (w *approvalPrompt) fieldRole(idx int) theme.Role {
	if w.focus == idx {
		return theme.Trust
	}
	return theme.Muted
}

// fieldText renders one field's content with a trailing cursor when it is
// focused, and a placeholder dash when an idle field is empty.
func (w *approvalPrompt) fieldText(idx int) string {
	val := w.scope
	if idx == 1 {
		val = w.feedback
	}
	if w.focus == idx {
		return val + "▏"
	}
	if val == "" {
		return "-"
	}
	return val
}
