// Package session is the agent's conversational front door: a streaming Session
// over the goal runtime that a chat UI drives turn by turn and renders live. It is
// a new host boundary alongside state and observe, the "streams/sessions/chat"
// surface.
//
// A Session exposes the mission event spine as a live, replayable event stream.
// Submit a goal and the session opens, then every model turn, the model's text,
// each tool call and its result, and the terminal outcome are emitted as ordered
// session.Events. The events are durable on a spine stream and fanned out over the
// bus, so a subscriber that joins late replays the conversation from the start and
// then tails it live. This is the reusable, embeddable generalization of the
// terminal-first goal runner: the same run, surfaced as a stream a panel renders.
//
// The session does not own the model loop. A caller wires the session's Reporter
// into a mission executor (mission.WithObserver) and hands the assembled runtime
// to Submit, so the agent's behaviour and the streaming surface stay decoupled.
package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ionalpha/flynn/bus"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/runtime"
	"github.com/ionalpha/flynn/spine"
)

// DefaultPoll is how often the session re-reads the goal's status to detect the
// terminal converged or stalled phase, the always-correct floor beneath the live
// turn events the mission reports directly.
const DefaultPoll = 50 * time.Millisecond

// Session is one conversational run: its event stream, and the lifecycle watch
// that turns the goal reaching a terminal phase into a terminal event and a result
// a caller can await. Construct it with New, feed its Reporter into a mission
// executor, then drive it with Submit.
type Session struct {
	stream *Stream
	poll   time.Duration

	mu             sync.Mutex
	result         string
	failed         error
	done           chan struct{}
	maxTurnStarted int
}

// Option configures a Session.
type Option func(*Session)

// WithID sets the session's identity, which names its spine stream and bus
// subject. A host embeds its own conversation id here so the stream lines up with
// its records. The id must be a valid bus subject token (no whitespace, dots, or
// wildcards). The default is a freshly generated UUIDv7, globally unique and
// stable across restarts so a run's event stream stays addressable.
func WithID(id string) Option {
	return func(s *Session) {
		if id != "" {
			poll := s.stream.poll
			s.stream = newStream(s.stream.log, s.stream.bus, id)
			s.stream.poll = poll
		}
	}
}

// WithPollInterval overrides how often the lifecycle watch re-reads goal status.
func WithPollInterval(d time.Duration) Option {
	return func(s *Session) {
		if d > 0 {
			s.poll = d
		}
	}
}

// WithStreamPoll overrides a subscriber's fallback re-read interval (see
// DefaultStreamPoll). A non-positive value disables the floor, leaving the bus the sole
// liveness signal, which a host should do only when it trusts the bus to deliver.
func WithStreamPoll(d time.Duration) Option {
	return func(s *Session) { s.stream.poll = d }
}

// New builds a Session whose events are recorded on log and fanned out over b.
// Its identity defaults to a fresh UUIDv7 (override with WithID); that id names
// the run's event stream and, via Submit, its goal resource, so one value
// addresses the whole run for replay, audit, and sync.
func New(log spine.Log, b bus.Bus, opts ...Option) *Session {
	s := &Session{
		stream: newStream(log, b, ids.New()),
		poll:   DefaultPoll,
		done:   make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ID returns the session's identity (its spine stream and bus subject suffix).
func (s *Session) ID() string { return s.stream.stream }

// Reporter returns the mission.Reporter that records the conversation onto this
// session's stream. Wire it into the executor with mission.WithObserver before
// handing the runtime to Submit.
func (s *Session) Reporter() mission.Reporter { return reporter{s} }

// Subscribe returns a live, ordered event channel beginning after afterSeq (0
// replays from the start). See Stream.Subscribe for the delivery guarantees.
func (s *Session) Subscribe(ctx context.Context, afterSeq int64) (<-chan Event, error) {
	return s.stream.Subscribe(ctx, afterSeq)
}

// History returns every recorded event for the run identified by id, read in order
// straight from the durable log. Unlike Subscribe it is finite: it does not wait
// for new events, so it is the primitive for inspecting or replaying a run that has
// already finished. An unknown id yields an empty slice, not an error.
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

// Submit opens the session, submits spec as a goal to rt, and starts watching it.
// The goal is named after the session id, so the run's event stream and its goal
// resource share one identity: the run is addressable by a single id for replay,
// audit, and (later) ownership and sync. It emits the session.started event,
// returns the goal's key, and from then on the session streams the conversation
// (via the Reporter the caller wired in) and, when the goal converges or stalls,
// emits the terminal event and releases Wait. The caller owns rt's lifecycle:
// rt.Start must be running for the goal to make progress.
func (s *Session) Submit(ctx context.Context, rt *runtime.Runtime, spec goal.Spec) (resource.Key, error) {
	s.emit(ctx, Event{Kind: KindSessionStarted, Actor: spine.ActorSystem, Text: spec.Objective})
	g, err := rt.SubmitGoal(ctx, s.ID(), spec)
	if err != nil {
		// The session opened but never started: release Wait with the failure rather
		// than leaving it to block until the context is cancelled.
		s.finish("", err)
		return resource.Key{}, err
	}
	key := g.Key()
	go s.watch(ctx, rt, key)
	return key, nil
}

// Resume attaches the session to an already-submitted goal identified by key and
// watches it to its terminal phase, without re-opening the session or
// re-submitting the goal. It is how a run is continued: the caller arranges for the
// runtime to re-drive the existing goal (see runtime.Resume), and the session
// streams the rest of the conversation onto the same stream as the original run. It
// emits no session.started, since the run was already opened.
func (s *Session) Resume(ctx context.Context, rt *runtime.Runtime, key resource.Key) {
	go s.watch(ctx, rt, key)
}

// Wait blocks until the session reaches a terminal state and returns the model's
// final answer on convergence, or a non-nil error on stall or cancellation.
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

// watch polls the goal's status until it reaches a terminal phase, then emits the
// matching terminal event and finishes the session. Polling is the robust floor:
// the per-turn events arrive live from the mission reporter, but the converged or
// stalled transition is a property of goal status the reconciler owns, so the
// session observes it the same way an external client would.
func (s *Session) watch(ctx context.Context, rt *runtime.Runtime, key resource.Key) {
	t := time.NewTicker(s.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finish("", ctx.Err())
			return
		case <-t.C:
			r, err := rt.Store().Get(ctx, key.Kind, key.Scope, key.Name)
			if err != nil {
				// A cancelled context surfaces here as a read error; the next loop
				// iteration observes ctx.Done and finishes. Any other read error is
				// transient, so retry on the next tick.
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
				// Pending/Running are not terminal; keep polling for a later phase.
			}
		}
	}
}

// finish records the terminal result and releases Wait exactly once; later calls
// (e.g. a cancellation racing a convergence) are no-ops.
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

// emit appends ev to the session's stream. It is best effort: the in-memory spine
// never errors, and a durable backend's append error is surfaced through the
// host's observability rather than failing the conversation.
func (s *Session) emit(ctx context.Context, ev Event) {
	_ = s.stream.append(ctx, ev)
}

// reporter adapts a Session to mission.Reporter, mapping each conversational event
// the mission loop reports onto the session's stream.
type reporter struct{ s *Session }

var _ mission.Reporter = reporter{}

func (r reporter) Report(ctx context.Context, ev mission.Event) {
	// A step that fails (a transient model error, say) is retried, re-running from
	// the same turn and re-announcing it. Recording the highest turn started makes
	// turn.started idempotent, so a retry neither spams the stream nor resets the
	// turn counter. The dedup is at write time, so a replay of the durable stream is
	// clean too.
	if ev.Kind == mission.EventTurnStarted && !r.s.advanceTurn(ev.Turn) {
		return
	}
	r.s.emit(ctx, toSessionEvent(ev))
}

// advanceTurn records that turn n has started and reports whether this is the
// first time, so a re-announced turn from a retry is dropped.
func (s *Session) advanceTurn(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= s.maxTurnStarted {
		return false
	}
	s.maxTurnStarted = n
	return true
}

// toSessionEvent maps a mission event to its session event. Every conversational
// event is the agent acting; session-level events (started, stalled) are set by
// the session itself.
func toSessionEvent(ev mission.Event) Event {
	out := Event{Actor: spine.ActorAgent, Turn: ev.Turn}
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
