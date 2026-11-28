package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	s.carriedTaint = nil
	s.projectRecorded = false
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
	if s.ext != nil {
		// The conversation being compacted is the CLI's, not ours: it holds the context and
		// manages it, and this side has no model to summarize through anyway. Say so rather
		// than silently compacting a transcript that is not the one the harness will read.
		return 0, errors.New("/compact is not available in a " + s.ext.driver.Name() + " session: the external agent holds the conversation and manages its own context")
	}
	summary, err := summarizeConversation(ctx, s.model, s.carriedContext, s.transcript)
	if err != nil {
		return 0, err
	}
	if summary == "" {
		return 0, errors.New("compaction produced an empty summary; leaving the conversation unchanged")
	}
	n := len(s.transcript)
	// The summary carries the compacted turns' content forward, so it carries their
	// taint too: the transcript the session distils from at the end will no longer
	// show the shell call that fetched what the summary repeats.
	carried := slices.Concat(s.carriedTaint, taintReasons(s.transcript))
	s.clear()
	s.carriedTaint = carried
	s.carriedContext = "Summary of the earlier conversation (compacted to save context):\n" + summary
	return n, nil
}

// summarizeConversation asks the model to compress a transcript into a concise summary
// that preserves the objective, decisions, established facts, changed state, and what
// remains, so the assistant can continue with far less context. earlier is the summary a
// previous compaction carried, which the new one must keep or it is lost on the second
// /compact.
//
// The transcript goes to the model as one text message, never as its own messages. It
// records the tool calls but not their results, and a provider rejects a tool_use block
// that no tool_result follows (Anthropic answers HTTP 400).
func summarizeConversation(ctx context.Context, model llm.Model, earlier string, transcript []llm.Message) (string, error) {
	var b strings.Builder
	if earlier != "" {
		b.WriteString(earlier)
		b.WriteString("\n\n")
	}
	b.WriteString("Conversation:\n")
	b.WriteString(transcriptText(transcript))
	b.WriteString("\nSummarize the conversation above as compactly as possible while losing nothing critical: the objective, decisions made, facts established, files or state changed, and what remains to do. Output only the summary.")
	resp, err := model.Generate(ctx, llm.Request{
		System:   "You compact a conversation into a concise summary that lets the assistant continue with far less context, preserving every critical detail.",
		Messages: []llm.Message{llm.Text(llm.RoleUser, b.String())},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(resp.Message.TextContent()), nil
}

// toolInputLimit bounds each tool call's arguments in [transcriptText], so one large
// write cannot fill the summarizer's context.
const toolInputLimit = 300

// transcriptText renders a transcript as plain lines, one per text block or tool call,
// with each call's arguments cut to [toolInputLimit] bytes.
func transcriptText(transcript []llm.Message) string {
	var b strings.Builder
	for _, m := range transcript {
		if text := strings.TrimSpace(m.TextContent()); text != "" {
			fmt.Fprintf(&b, "%s: %s\n", m.Role, text)
		}
		for _, tu := range m.ToolUses() {
			input := string(tu.Input)
			if len(input) > toolInputLimit {
				input = strings.ToValidUTF8(input[:toolInputLimit], "") + "..."
			}
			fmt.Fprintf(&b, "%s: [called %s %s]\n", m.Role, tu.Name, input)
		}
	}
	return b.String()
}
