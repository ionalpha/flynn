package session

import (
	"context"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

func newHeadlessSession(t *testing.T) (*Session, spine.Log) {
	t.Helper()
	log := spine.NewMemoryLog()
	return New(log, bus.NewMemory(), WithID("run-1")), log
}

func history(t *testing.T, log spine.Log) []Event {
	t.Helper()
	evs, err := History(context.Background(), log, "run-1")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	return evs
}

func ofKind(evs []Event, k Kind) []Event {
	var out []Event
	for _, e := range evs {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

// TestApprovalRecordsRoundTrip pins the two stored type strings to the ones the approval
// sink writes, by writing through the sink and decoding here: a rename on either side
// turns a request into a kind-less event and fails this.
func TestApprovalRecordsRoundTrip(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewApproval(log, "run-1")
	expires := time.Date(2026, 11, 14, 12, 0, 0, 123, time.UTC)
	scope := state.Scope{Instance: "i", Project: "p"}

	if err := sink.Request(ctx, spinesink.Request{ID: "req-1", Action: "shell", Scope: scope, Host: "box", Tainted: true, ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Answer(ctx, spinesink.Answer{RequestID: "req-1", Action: "shell", Scope: scope, Host: "box", Reason: approval.ReasonAllowed, Grant: "ls *", Client: "c1"}); err != nil {
		t.Fatal(err)
	}

	evs := history(t, log)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want a request and a decision", len(evs))
	}
	req, dec := evs[0], evs[1]
	if req.Kind != KindApprovalRequested || dec.Kind != KindApprovalDecision {
		t.Fatalf("kinds = %s, %s; the request must come first", req.Kind, dec.Kind)
	}
	if req.Seq >= dec.Seq {
		t.Fatalf("request seq %d is not before decision seq %d", req.Seq, dec.Seq)
	}
	if req.RequestID != "req-1" || req.Action != "shell" || req.Scope != "i/p" || req.Host != "box" || !req.Tainted || !req.ExpiresAt.Equal(expires) {
		t.Fatalf("request decoded wrong: %+v", req)
	}
	if req.Preview != `allow "shell" in scope "i/p" on host "box"` {
		t.Fatalf("preview = %q", req.Preview)
	}
	if req.Actor != spine.ActorSystem {
		t.Fatalf("request actor = %s, want system", req.Actor)
	}
	if dec.RequestID != "req-1" || dec.Outcome != approval.OutcomeAllow || dec.Reason != "allowed" || dec.Grant != "ls *" || dec.Client != "c1" || dec.Scope != "i/p" {
		t.Fatalf("decision decoded wrong: %+v", dec)
	}
	if dec.Actor != spine.ActorHuman {
		t.Fatalf("an allow by a person has actor %s, want human", dec.Actor)
	}
}

// TestExpiredPromptRecordsADecline is the sink acceptance: the request is on the stream
// before the decision, and an expiry is a decline with reason expired, written by the
// system and not attributed to a person.
func TestExpiredPromptRecordsADecline(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewApproval(log, "run-1")
	_ = sink.Request(ctx, spinesink.Request{ID: "r", Action: "shell"})
	_ = sink.Answer(ctx, spinesink.Answer{RequestID: "r", Action: "shell", Reason: approval.ReasonExpired})

	evs := history(t, log)
	if len(evs) != 2 || evs[0].Kind != KindApprovalRequested || evs[1].Kind != KindApprovalDecision {
		t.Fatalf("stream = %+v, want the request then the decision", evs)
	}
	d := evs[1]
	if d.Outcome != approval.OutcomeDeny || d.Reason != "expired" || d.Actor != spine.ActorSystem {
		t.Fatalf("expired decision = %+v", d)
	}
}

// TestResolutionActorPerReason: an answer from a person is human, a resolution the system
// made is system, for every value of the closed set.
func TestResolutionActorPerReason(t *testing.T) {
	want := map[approval.Reason]spine.ActorType{
		approval.ReasonAllowed:      spine.ActorHuman,
		approval.ReasonDenied:       spine.ActorHuman,
		approval.ReasonExpired:      spine.ActorSystem,
		approval.ReasonCancelled:    spine.ActorSystem,
		approval.ReasonDisconnected: spine.ActorSystem,
	}
	if len(want) != len(approval.Reasons()) {
		t.Fatalf("this table covers %d reasons, the closed set has %d", len(want), len(approval.Reasons()))
	}
	for reason, actor := range want {
		t.Run(string(reason), func(t *testing.T) {
			log := spine.NewMemoryLog()
			sink := spinesink.NewApproval(log, "run-1")
			if err := sink.Answer(context.Background(), spinesink.Answer{RequestID: "r", Action: "shell", Reason: reason}); err != nil {
				t.Fatal(err)
			}
			evs := history(t, log)
			if len(evs) != 1 || evs[0].Actor != actor {
				t.Fatalf("actor = %+v, want %s", evs, actor)
			}
			wantOutcome := approval.OutcomeDeny
			if reason == approval.ReasonAllowed {
				wantOutcome = approval.OutcomeAllow
			}
			if evs[0].Outcome != wantOutcome {
				t.Fatalf("outcome = %s, want %s", evs[0].Outcome, wantOutcome)
			}
		})
	}
}

// TestAnswerRefusesAnyOtherReason is the closed-enum table: a value outside the five is
// refused and nothing reaches the stream.
func TestAnswerRefusesAnyOtherReason(t *testing.T) {
	for _, bad := range []approval.Reason{"", "timeout", "Allowed", "approved", "allowed\n", "expired ", "granted", "error"} {
		t.Run(string(bad), func(t *testing.T) {
			log := spine.NewMemoryLog()
			sink := spinesink.NewApproval(log, "run-1")
			if err := sink.Answer(context.Background(), spinesink.Answer{RequestID: "r", Action: "shell", Reason: bad}); err == nil {
				t.Fatal("a reason outside the closed set was accepted")
			}
			if n := len(history(t, log)); n != 0 {
				t.Fatalf("a refused answer left %d events on the stream", n)
			}
		})
	}
}

// TestDecodeApprovalFailsClosed: a stored resolution allows only when its outcome and its
// reason agree. A forged or damaged record that says allow with another reason, or that
// carries a reason nobody defined, reads as a denial.
func TestDecodeApprovalFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		outcome string
		reason  string
		want    approval.Outcome
	}{
		{"allow with allowed", "allow", "allowed", approval.OutcomeAllow},
		{"allow with expired", "allow", "expired", approval.OutcomeDeny},
		{"allow with unknown reason", "allow", "because", approval.OutcomeDeny},
		{"allow with no reason", "allow", "", approval.OutcomeDeny},
		{"unknown outcome", "maybe", "allowed", approval.OutcomeDeny},
		{"deny", "deny", "denied", approval.OutcomeDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := fromSpine(spine.Event{Type: typeApprovalDecision, Payload: map[string]any{"outcome": tc.outcome, "reason": tc.reason, "requestId": "r"}})
			if e.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", e.Outcome, tc.want)
			}
		})
	}
}

// TestDecodeIgnoresUnknownKeys: a payload carrying a key this version does not know still
// decodes, for each of the new kinds.
func TestDecodeIgnoresUnknownKeys(t *testing.T) {
	cases := []struct {
		name string
		se   spine.Event
		want Kind
		chk  func(Event) bool
	}{
		{
			"requested",
			spine.Event{Type: typeApprovalRequested, Payload: map[string]any{"requestId": "r", "action": "shell", "future": []any{1, 2}}},
			KindApprovalRequested,
			func(e Event) bool { return e.RequestID == "r" && e.Action == "shell" },
		},
		{
			"decision",
			spine.Event{Type: typeApprovalDecision, Payload: map[string]any{"outcome": "deny", "reason": "denied", "future": map[string]any{"a": 1}}},
			KindApprovalDecision,
			func(e Event) bool { return e.Outcome == approval.OutcomeDeny && e.Reason == "denied" },
		},
		{
			"prompt ended",
			spine.Event{Type: string(KindPromptEnded), Payload: map[string]any{payloadKey: `{"promptId":"p","stop":"converged","steps":3,"future":true}`}},
			KindPromptEnded,
			func(e Event) bool { return e.PromptID == "p" && e.Stop == StopConverged && e.Steps == 3 },
		},
		{
			"client attached",
			spine.Event{Type: string(KindClientAttached), Payload: map[string]any{payloadKey: `{"attachment":{"id":"c","major":1,"attested":true,"future":1}}`}},
			KindClientAttached,
			func(e Event) bool { return e.Attachment != nil && e.Attachment.ID == "c" && !e.Attachment.Attested },
		},
		{
			"budget extended",
			spine.Event{Type: string(KindBudgetExtended), Payload: map[string]any{payloadKey: `{"budget":{"old":{"tokens":10},"new":{"tokens":20},"total":{"tokens":10},"future":1}}`}},
			KindBudgetExtended,
			func(e Event) bool { return e.Budget != nil && e.Budget.New.Tokens == 20 },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := fromSpine(tc.se)
			if e.Kind != tc.want || !tc.chk(e) {
				t.Fatalf("decoded %+v", e)
			}
		})
	}
}

// TestDecodeLegacyApprovalVerdict: the gate's own verdict record, written before any
// prompt, still decodes, and is told apart from a resolution by having no request id and
// no reason.
func TestDecodeLegacyApprovalVerdict(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewApproval(log, "run-1")
	_ = sink.Record(ctx, approval.Decision{
		Granted:  false,
		Reason:   "approval: needs one signature\x1b[2K",
		KeyIDs:   nil,
		Envelope: approval.Envelope{Action: "shell", Detail: "rm -rf x"},
	})
	evs := history(t, log)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	e := evs[0]
	if e.Kind != KindApprovalDecision || e.Outcome != approval.OutcomeDeny || e.RequestID != "" || e.Reason != "" {
		t.Fatalf("verdict = %+v", e)
	}
	if e.Action != "shell" || e.Grant != "rm -rf x" {
		t.Fatalf("verdict fields = %+v", e)
	}
	if strings.ContainsRune(e.Text, 0x1b) || !strings.Contains(e.Text, `\x1b`) {
		t.Fatalf("the gate's free-text reason was not made inert: %q", e.Text)
	}
}

// TestPreviewIsInertAndBounded: a request whose parts carry control characters or run
// long reaches the stream as inert, bounded text, from the sink and again on decode.
func TestPreviewIsInertAndBounded(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	sink := spinesink.NewApproval(log, "run-1")
	long := strings.Repeat("a", 10_000)
	_ = sink.Request(ctx, spinesink.Request{ID: "r", Action: "shell\x1b[31m\rEVIL\u202e", Host: long})
	e := history(t, log)[0]
	if len(e.Preview) > approval.PreviewLimit {
		t.Fatalf("preview is %d bytes, limit %d", len(e.Preview), approval.PreviewLimit)
	}
	for _, r := range e.Preview + e.Action {
		if r == 0x1b || r == '\r' || r == '\u202e' {
			t.Fatalf("an unescaped %U reached the stream: %q", r, e.Preview)
		}
	}
	if !strings.Contains(e.Preview, `\x1b[31m\x0dEVIL`+`\u202e`) {
		t.Fatalf("the escapes are not visible: %q", e.Preview)
	}

	// A record some other writer made reaches a client inert as well.
	raw := fromSpine(spine.Event{Type: typeApprovalRequested, Payload: map[string]any{"preview": "ok\x1b]0;title\x07"}})
	if strings.ContainsAny(raw.Preview, "\x1b\x07") {
		t.Fatalf("decode passed control bytes through: %q", raw.Preview)
	}
}

// TestRunPromptEndsEveryPromptOnce is the exit-path table: a prompt that converges, stalls,
// stops on budget, is killed, is cancelled, fails, panics or has its goroutine exit each
// leaves exactly one prompt.ended, with the stop that path means.
func TestRunPromptEndsEveryPromptOnce(t *testing.T) {
	usage := &Usage{InputTokens: 12, OutputTokens: 3}
	cases := []struct {
		name string
		run  func(ctx context.Context, cancel context.CancelFunc) (PromptEnd, error)
		want Stop
	}{
		{"converged", func(context.Context, context.CancelFunc) (PromptEnd, error) {
			return PromptEnd{Stop: StopConverged, Steps: 4, Usage: usage}, nil
		}, StopConverged},
		{"stalled", func(context.Context, context.CancelFunc) (PromptEnd, error) {
			return PromptEnd{Stop: StopStalled, Reason: "no progress"}, nil
		}, StopStalled},
		{"budget", func(context.Context, context.CancelFunc) (PromptEnd, error) {
			return PromptEnd{Stop: StopBudgetExceeded}, nil
		}, StopBudgetExceeded},
		{"killed", func(context.Context, context.CancelFunc) (PromptEnd, error) {
			return PromptEnd{Stop: StopKilled}, nil
		}, StopKilled},
		{"cancelled by context", func(ctx context.Context, cancel context.CancelFunc) (PromptEnd, error) {
			cancel()
			return PromptEnd{}, ctx.Err()
		}, StopCancelled},
		{"error", func(context.Context, context.CancelFunc) (PromptEnd, error) {
			return PromptEnd{}, errors.New("model unreachable")
		}, StopError},
		{"invalid stop is an error", func(context.Context, context.CancelFunc) (PromptEnd, error) {
			return PromptEnd{Stop: "finished"}, nil
		}, StopError},
		{"panic", func(context.Context, context.CancelFunc) (PromptEnd, error) {
			panic("boom")
		}, StopError},
		{"goroutine exit", func(context.Context, context.CancelFunc) (PromptEnd, error) {
			runtime.Goexit()
			return PromptEnd{}, nil
		}, StopError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, log := newHeadlessSession(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = recover() }()
				_, _ = s.RunPrompt(ctx, "p1", func(ctx context.Context) (PromptEnd, error) { return tc.run(ctx, cancel) })
			}()
			<-done

			ended := ofKind(history(t, log), KindPromptEnded)
			if len(ended) != 1 {
				t.Fatalf("got %d prompt.ended, want exactly 1", len(ended))
			}
			e := ended[0]
			if e.PromptID != "p1" || e.Stop != tc.want || !e.Stop.Valid() || e.Actor != spine.ActorSystem {
				t.Fatalf("prompt.ended = %+v, want stop %s", e, tc.want)
			}
		})
	}
}

// TestPromptEndedCarriesUsageAndSteps checks the payload a normal ending keeps.
func TestPromptEndedCarriesUsageAndSteps(t *testing.T) {
	s, log := newHeadlessSession(t)
	u := &Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60}
	_, _ = s.RunPrompt(context.Background(), "p", func(context.Context) (PromptEnd, error) {
		return PromptEnd{Stop: StopConverged, Steps: 7, Usage: u, Reason: "done\x1b[0m"}, nil
	})
	e := ofKind(history(t, log), KindPromptEnded)[0]
	if e.Steps != 7 || !reflect.DeepEqual(e.Usage, u) {
		t.Fatalf("steps/usage = %d, %+v", e.Steps, e.Usage)
	}
	if strings.ContainsRune(e.Reason, 0x1b) {
		t.Fatalf("reason is not inert: %q", e.Reason)
	}
}

// TestEndPromptIsOncePerPrompt: a cancellation racing a normal finish, or a repeated id,
// writes one ending.
func TestEndPromptIsOncePerPrompt(t *testing.T) {
	s, log := newHeadlessSession(t)
	ctx := context.Background()
	if !s.EndPrompt(ctx, "p", PromptEnd{Stop: StopConverged}) {
		t.Fatal("the first ending was not written")
	}
	if s.EndPrompt(ctx, "p", PromptEnd{Stop: StopCancelled}) {
		t.Fatal("a second ending for the same prompt was written")
	}
	if s.EndPrompt(ctx, "", PromptEnd{Stop: StopConverged}) {
		t.Fatal("an ending with no prompt id was written")
	}
	ended := ofKind(history(t, log), KindPromptEnded)
	if len(ended) != 1 || ended[0].Stop != StopConverged {
		t.Fatalf("endings = %+v, want the first one only", ended)
	}
}

// TestAttachClientIsSystemAndUnattested: the record is the system's, and the claimed
// identity is marked unattested whatever the caller said.
func TestAttachClientIsSystemAndUnattested(t *testing.T) {
	s, log := newHeadlessSession(t)
	s.AttachClient(context.Background(), ClientAttachment{
		ID: "c1", Major: 1, Capabilities: []string{"approvals", "usage"}, Transport: "stdio", PeerPID: 4242,
		ClaimedName: "editor\x1b[2K", ClaimedVersion: "9.9", Attested: true,
	})
	e := ofKind(history(t, log), KindClientAttached)[0]
	a := e.Attachment
	if e.Actor != spine.ActorSystem || a == nil {
		t.Fatalf("attached = %+v", e)
	}
	if a.Attested {
		t.Fatal("a claimed identity was recorded as attested")
	}
	if a.Major != 1 || a.Transport != "stdio" || a.PeerPID != 4242 || !reflect.DeepEqual(a.Capabilities, []string{"approvals", "usage"}) || e.Client != "c1" {
		t.Fatalf("attachment = %+v", a)
	}
	if strings.ContainsRune(a.ClaimedName, 0x1b) {
		t.Fatalf("the claimed name is not inert: %q", a.ClaimedName)
	}
}

// TestExtendBudget: a raise is recorded by a person with the old ceiling, the new one and
// the running total; anything that is not a raise is refused and records nothing.
func TestExtendBudget(t *testing.T) {
	s, log := newHeadlessSession(t)
	ctx := context.Background()

	if err := s.ExtendBudget(ctx, Ceiling{Tokens: 100, Cost: 1}, Ceiling{Tokens: 150, Cost: 1.5}); err != nil {
		t.Fatal(err)
	}
	if err := s.ExtendBudget(ctx, Ceiling{Tokens: 150, Cost: 1.5}, Ceiling{Tokens: 400, Cost: 1.5}); err != nil {
		t.Fatal(err)
	}
	refused := []struct {
		name      string
		old, next Ceiling
	}{
		{"no change", Ceiling{Tokens: 100}, Ceiling{Tokens: 100}},
		{"lowers tokens", Ceiling{Tokens: 100}, Ceiling{Tokens: 99}},
		{"lowers cost", Ceiling{Cost: 2}, Ceiling{Cost: 1}},
		{"raises one lowers the other", Ceiling{Tokens: 10, Cost: 2}, Ceiling{Tokens: 20, Cost: 1}},
		{"negative start", Ceiling{Tokens: -5}, Ceiling{Tokens: 5}},
		{"NaN cost", Ceiling{Cost: 1}, Ceiling{Cost: math.NaN()}},
		{"infinite cost", Ceiling{Cost: 1}, Ceiling{Cost: math.Inf(1)}},
	}
	for _, tc := range refused {
		if err := s.ExtendBudget(ctx, tc.old, tc.next); err == nil {
			t.Errorf("%s: a change that is not a raise was recorded", tc.name)
		}
	}

	got := ofKind(history(t, log), KindBudgetExtended)
	if len(got) != 2 {
		t.Fatalf("got %d budget.extended, want only the two raises", len(got))
	}
	last := got[1]
	if last.Actor != spine.ActorHuman {
		t.Fatalf("actor = %s, want human", last.Actor)
	}
	want := BudgetExtension{Old: Ceiling{Tokens: 150, Cost: 1.5}, New: Ceiling{Tokens: 400, Cost: 1.5}, Total: Ceiling{Tokens: 300, Cost: 0.5}}
	if *last.Budget != want {
		t.Fatalf("budget = %+v, want %+v", *last.Budget, want)
	}
}

// TestNewKindsDoNotChangeProjection: a session that holds the new kinds projects exactly
// as it does without them.
func TestNewKindsDoNotChangeProjection(t *testing.T) {
	base := []Event{
		{Seq: 1, Kind: KindSessionStarted, Text: "go"},
		{Seq: 2, Kind: KindTurnStarted, Turn: 1},
		{Seq: 3, Kind: KindAssistant, Turn: 1, Text: "hi"},
		{Seq: 4, Kind: KindTurnCompleted, Turn: 1},
		{Seq: 5, Kind: KindConverged, Text: "done"},
	}
	extra := []Event{
		{Seq: 2, Kind: KindClientAttached, Attachment: &ClientAttachment{ID: "c"}},
		{Seq: 3, Kind: KindApprovalRequested, RequestID: "r"},
		{Seq: 4, Kind: KindApprovalDecision, RequestID: "r", Outcome: approval.OutcomeDeny, Reason: "denied"},
		{Seq: 5, Kind: KindBudgetExtended, Budget: &BudgetExtension{}},
		{Seq: 6, Kind: KindPromptEnded, PromptID: "p", Stop: StopConverged},
	}
	mixed := append([]Event{base[0]}, extra[:2]...)
	mixed = append(mixed, base[1:3]...)
	mixed = append(mixed, extra[2:]...)
	mixed = append(mixed, base[3:]...)
	if !reflect.DeepEqual(Project(base), Project(mixed)) {
		t.Fatalf("the new kinds changed the projection:\n%+v\n%+v", Project(base), Project(mixed))
	}
}

func TestStopIsAClosedSet(t *testing.T) {
	want := []string{"converged", "stalled", "cancelled", "budget_exceeded", "killed", "error"}
	got := Stops()
	if len(got) != len(want) {
		t.Fatalf("Stops = %v", got)
	}
	for i, s := range want {
		if string(got[i]) != s || !got[i].Valid() {
			t.Fatalf("Stops[%d] = %q, want %q", i, got[i], s)
		}
	}
	for _, bad := range []Stop{"", "done", "Converged", "cancel", "timeout"} {
		if bad.Valid() {
			t.Errorf("%q is valid", bad)
		}
	}
}
