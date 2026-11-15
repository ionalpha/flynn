// Package session streams a goal run as ordered [Event] values that a chat UI
// renders live and a late subscriber replays from the start.
//
// Events are recorded on a spine stream and woken over the bus: every model
// turn, the model's text, each tool call and result, and the terminal outcome.
// The session does not own the model loop. A caller wires [Session.Reporter]
// into a mission executor (mission.WithObserver) and hands the runtime to
// [Session.Submit], so the agent's behaviour and the stream stay independent.
package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/spine"
)

// DefaultPoll is how often the session re-reads goal status to detect the
// converged or stalled phase. The reconciler sets that phase, so polling is
// the floor beneath the live turn events.
const DefaultPoll = 50 * time.Millisecond

// Session is one conversational run: its event stream and the watch that turns
// a terminal goal phase into a terminal event and a result for Wait. Build it
// with New, wire its Reporter into a mission executor, then call Submit.
type Session struct {
	stream *Stream
	poll   time.Duration
	clk    clock.Timing

	mu             sync.Mutex
	result         string
	failed         error
	done           chan struct{}
	maxTurnStarted map[string]int
	// ended holds prompt ids that already have a prompt.ended record, so a prompt
	// is closed once however many paths close it. raised totals every budget
	// extension recorded.
	ended  map[string]struct{}
	raised Ceiling
}

// Option configures a [Session] at construction.
type Option func(*Session)

// WithID sets the session id, which names its spine stream and bus subject, so
// a host can align the stream with its own conversation id. The id must be a
// valid bus subject token (no whitespace, dots or wildcards). Default: a fresh
// UUIDv7.
func WithID(id string) Option {
	return func(s *Session) {
		if id != "" {
			poll := s.stream.poll
			clk := s.stream.clk
			s.stream = newStream(s.stream.log, s.stream.bus, id)
			s.stream.poll = poll
			s.stream.clk = clk
		}
	}
}

// WithPollInterval sets how often the lifecycle watch re-reads goal status.
func WithPollInterval(d time.Duration) Option {
	return func(s *Session) {
		if d > 0 {
			s.poll = d
		}
	}
}

// WithStreamPoll sets a subscriber's fallback re-read interval (see
// [DefaultStreamPoll]). A non-positive value leaves the bus as the only wake,
// which a host should choose only when it trusts the bus to deliver.
func WithStreamPoll(d time.Duration) Option {
	return func(s *Session) { s.stream.poll = d }
}

// WithClock sets the clock for the lifecycle watch and the subscriber poll
// floor, so a test drives them with a Manual clock instead of sleeping.
func WithClock(clk clock.Timing) Option {
	return func(s *Session) {
		if clk != nil {
			s.clk = clk
			s.stream.clk = clk
		}
	}
}

// New builds a Session recorded on log and woken over b. The session id also
// names the goal Submit creates, so one value addresses the run for replay,
// audit and sync.
func New(log spine.Log, b bus.Bus, opts ...Option) *Session {
	s := &Session{
		stream: newStream(log, b, ids.New()),
		poll:   DefaultPoll,
		clk:    clock.System{},
		done:   make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ID returns the session id: its spine stream and bus subject suffix.
func (s *Session) ID() string { return s.stream.stream }

// Reporter records the conversation onto this session's stream. Wire it with
// mission.WithObserver before calling Submit.
func (s *Session) Reporter() mission.Reporter { return reporter{s} }

// Subscribe returns a live, ordered event channel starting after afterSeq (0
// replays from the start). See [Stream.Subscribe] for the guarantees.
func (s *Session) Subscribe(ctx context.Context, afterSeq int64) (<-chan Event, error) {
	return s.stream.Subscribe(ctx, afterSeq)
}

// History returns every recorded event for run id, in order, from the log. It
// does not wait for new events, so it suits a finished run. An unknown id
// yields an empty slice.
func History(ctx context.Context, log spine.Log, id string) ([]Event, error) {
	recs, err := log.Read(ctx, spine.Query{Stream: id})
	if err != nil {
		return nil, err
	}
	out := make([]Event, len(recs))
	for i, se := range recs {
		out[i] = fromSpine(se)
	}
	return out, nil
}

// Submit emits session.started, submits spec to rt as a goal named after the
// session id, and returns the goal's key. The session then streams the
// conversation through the wired Reporter and, when the goal converges or
// stalls, emits the terminal event and releases Wait. The caller owns rt:
// rt.Start must be running for the goal to progress.
func (s *Session) Submit(ctx context.Context, rt *runtime.Runtime, spec goal.Spec) (resource.Key, error) {
	s.emit(ctx, Event{Kind: KindSessionStarted, Actor: spine.ActorSystem, Text: spec.Objective})
	g, err := rt.SubmitGoal(ctx, s.ID(), spec)
	if err != nil {
		// Release Wait with the failure; otherwise it blocks until ctx is cancelled.
		s.finish("", err)
		return resource.Key{}, err
	}
	key := g.Key()
	go s.watch(ctx, rt, key)
	return key, nil
}

// Resume watches an already-submitted goal to its terminal phase, appending to
// the original stream without emitting session.started. The caller arranges
// for the runtime to re-drive the goal (see runtime.Resume).
func (s *Session) Resume(ctx context.Context, rt *runtime.Runtime, key resource.Key) {
	go s.watch(ctx, rt, key)
}

// Wait returns the final answer on convergence, or an error on stall or
// cancellation.
func (s *Session) Wait(ctx context.Context) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.result, s.failed
	}
}

// watch polls goal status until a terminal phase, then emits the terminal event
// and finishes. Turn events arrive live from the reporter, but convergence is a
// goal status the reconciler owns, so the session reads it as an external
// client would.
func (s *Session) watch(ctx context.Context, rt *runtime.Runtime, key resource.Key) {
	for {
		t := s.clk.NewTimer(s.poll)
		select {
		case <-ctx.Done():
			t.Stop()
			s.finish("", ctx.Err())
			return
		case <-t.C():
			r, err := rt.Store().Get(ctx, key.Kind, key.Scope, key.Name)
			if err != nil {
				// A cancelled context shows up as a read error and the next iteration sees
				// ctx.Done. Any other read error is transient.
				continue
			}
			st, err := goal.DecodeStatus(r)
			if err != nil {
				continue
			}
			switch st.Phase {
			case goal.PhaseConverged:
				s.emit(ctx, Event{Kind: KindConverged, Actor: spine.ActorAgent, Text: st.Message})
				s.finish(st.Message, nil)
				return
			case goal.PhaseStalled:
				s.emit(ctx, Event{Kind: KindStalled, Actor: spine.ActorSystem, Err: st.Message})
				s.finish("", fmt.Errorf("goal stalled: %s", st.Message))
				return
			default:
				// Pending and Running are not terminal.
			}
		}
	}
}

// finish records the result and releases Wait once; a later call (a
// cancellation racing a convergence) does nothing.
func (s *Session) finish(result string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return
	default:
	}
	s.result, s.failed = result, err
	close(s.done)
}

// emit appends ev to the stream. An append error goes to the host's
// observability and does not fail the conversation.
func (s *Session) emit(ctx context.Context, ev Event) {
	_ = s.stream.append(ctx, ev)
}

// reporter adapts a Session to mission.Reporter.
type reporter struct{ s *Session }

var _ mission.Reporter = reporter{}

func (r reporter) Report(ctx context.Context, ev mission.Event) {
	// A failed step is retried from the same turn and re-announces it. Dropping a
	// turn.started at or below the highest seen keeps the counter and the stream
	// clean, and doing it at write time keeps a replay clean too.
	if ev.Kind == mission.EventTurnStarted && !r.s.advanceTurn(ev.Goal, ev.Turn) {
		return
	}
	r.s.emit(ctx, toSessionEvent(ev))
}

// advanceTurn records that turn n started for goal and reports whether it is
// the first time. The mark is per goal because under fan-out several goals
// share a stream and each numbers turns from one. An empty goal is the root.
func (s *Session) advanceTurn(goal string, n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= s.maxTurnStarted[goal] {
		return false
	}
	if s.maxTurnStarted == nil {
		s.maxTurnStarted = make(map[string]int)
	}
	s.maxTurnStarted[goal] = n
	return true
}

// toSessionEvent maps a mission event to a session event. Session-level events
// (started, stalled) are emitted by the session itself.
func toSessionEvent(ev mission.Event) Event {
	out := Event{Actor: spine.ActorAgent, Goal: ev.Goal, Turn: ev.Turn}
	switch ev.Kind {
	case mission.EventTurnStarted:
		out.Kind = KindTurnStarted
	case mission.EventAssistantText:
		out.Kind, out.Text = KindAssistant, ev.Text
	case mission.EventToolCall:
		out.Kind, out.Tool, out.ToolUseID, out.Input = KindToolCall, ev.Tool, ev.ToolUseID, ev.Input
	case mission.EventToolResult:
		out.Kind, out.Tool, out.ToolUseID = KindToolResult, ev.Tool, ev.ToolUseID
		out.Result, out.IsError = ev.Result, ev.IsError
	case mission.EventChildSpawned:
		out.Kind, out.Child, out.Tool, out.ToolUseID, out.Text = KindChildSpawned, ev.Child, ev.Tool, ev.ToolUseID, ev.Text
	case mission.EventChildCompleted:
		out.Kind, out.Child, out.Tool, out.ToolUseID = KindChildCompleted, ev.Child, ev.Tool, ev.ToolUseID
		out.Result, out.IsError = ev.Result, ev.IsError
	case mission.EventTurnCompleted:
		out.Kind, out.StopReason = KindTurnCompleted, ev.StopReason
		if u := ev.Usage; u != (llm.Usage{}) {
			out.Usage = &Usage{
				InputTokens:      u.InputTokens,
				OutputTokens:     u.OutputTokens,
				CacheReadTokens:  u.CacheReadTokens,
				CacheWriteTokens: u.CacheWriteTokens,
			}
		}
	}
	return out
}
