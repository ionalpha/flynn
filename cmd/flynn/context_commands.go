package main

import (
	"context"
	"errors"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// clear detaches the session from its current run and resets it to a fresh
// conversation: the next turn opens a new run. The prior run stays durable and
// resumable; clear only forgets it for this session, and drops any carried summary.
func (s *replSession) clear() {
	s.started = false
	s.runID = ""
	s.lastSeq = 0
	s.objective = ""
	s.converged = false
	s.transcript = nil
	s.recalled = nil
	s.lastResult = ""
	s.carriedContext = ""
}

// compact summarizes the conversation so far and resets the session to continue from
// that summary: the next turn opens a fresh run whose standing instructions carry the
// summary, so a long session stops resending its whole history. It returns how many
// messages were summarized. It needs a started run with a transcript; without one there
// is nothing to compact.
func (s *replSession) compact(ctx context.Context) (int, error) {
	if !s.started || len(s.transcript) == 0 {
		return 0, errors.New("nothing to compact yet; run a turn first")
	}
	summary, err := summarizeConversation(ctx, s.model, s.transcript)
	if err != nil {
		return 0, err
	}
	if summary == "" {
		return 0, errors.New("compaction produced an empty summary; leaving the conversation unchanged")
	}
	n := len(s.transcript)
	s.clear()
	s.carriedContext = "Summary of the earlier conversation (compacted to save context):\n" + summary
	return n, nil
}

// summarizeConversation asks the model to compress a transcript into a concise summary
// that preserves the objective, decisions, established facts, changed state, and what
// remains, so the assistant can continue with far less context.
func summarizeConversation(ctx context.Context, model llm.Model, transcript []llm.Message) (string, error) {
	msgs := make([]llm.Message, 0, len(transcript)+1)
	msgs = append(msgs, transcript...)
	msgs = append(msgs, llm.Text(llm.RoleUser, "Summarize the conversation so far as compactly as possible while losing nothing critical: the objective, decisions made, facts established, files or state changed, and what remains to do. Output only the summary."))
	resp, err := model.Generate(ctx, llm.Request{
		System:   "You compact a conversation into a concise summary that lets the assistant continue with far less context, preserving every critical detail.",
		Messages: msgs,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Message.TextContent()), nil
}
