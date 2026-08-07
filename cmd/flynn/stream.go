package main

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/session"
)

// renderStream prints the session's events as they arrive and accumulates the
// conversation transcript (the model's text and the tools it called), returning
// once the session reaches a terminal event: the model's summary on convergence,
// or an error on stall. lastSeq is the sequence of the last event consumed, so a
// caller tailing the same stream across turns can resume after it. A closed channel
// before any terminal event means the run was cancelled.
//
// observe, when non-nil, is called with every event before it is rendered to out.
// It is the tap an interactive client uses to render the typed stream itself (a
// themed transcript, a status badge) rather than the flat text this writes; the
// text path still runs, so a caller that wants only the typed events points out at
// io.Discard.
func renderStream(out io.Writer, events <-chan session.Event, verbose bool, observe func(session.Event)) (result string, transcript []llm.Message, lastSeq int64, err error) {
	var meter usageMeter
	for ev := range events {
		lastSeq = ev.Seq
		if observe != nil {
			observe(ev)
		}
		renderEvent(out, ev, verbose)
		if ev.Usage != nil {
			meter.add(*ev.Usage)
		}
		switch ev.Kind {
		case session.KindAssistant:
			transcript = append(transcript, llm.Text(llm.RoleAssistant, ev.Text))
		case session.KindToolCall:
			transcript = append(transcript, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: ev.ToolUseID, Name: ev.Tool, Input: ev.Input}},
			}})
		case session.KindConverged:
			renderUsageSummary(out, meter)
			return ev.Text, transcript, lastSeq, nil
		case session.KindStalled:
			// Show the spend even on failure: a run that stalled still cost tokens,
			// and that is exactly when the number is worth seeing.
			renderUsageSummary(out, meter)
			return "", transcript, lastSeq, fmt.Errorf("goal stalled: %s", ev.Err)
		default:
			// Already drawn by renderEvent above; only the kinds that build the
			// transcript or end the stream need handling here.
		}
	}
	return "", transcript, lastSeq, context.Canceled
}

// syncWriter serializes writes, so the stream-rendering goroutine and any other
// writer never interleave or race on the underlying writer.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
