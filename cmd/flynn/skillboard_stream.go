package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"github.com/ionalpha/flynn/usage"
)

// claudeCodeRun is what a bare Claude Code run reported in its stream-json output.
// Tokens are per model because Claude Code may hand side tasks to a smaller model than
// the one named, and each model is priced at its own rate.
type claudeCodeRun struct {
	finished    bool
	isError     bool
	subtype     string
	result      string
	turns       int
	firstUseful int
	reportedUSD float64
	hasCost     bool
	perModel    map[string]usage.Tokens
}

// claudeEditTools are Claude Code's tools that change a file, the ones a first useful
// action is counted at.
var claudeEditTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

type claudeStreamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message *struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        *struct {
		Input       int64 `json:"input_tokens"`
		Output      int64 `json:"output_tokens"`
		CacheRead   int64 `json:"cache_read_input_tokens"`
		CacheCreate int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		Input       int64 `json:"inputTokens"`
		Output      int64 `json:"outputTokens"`
		CacheRead   int64 `json:"cacheReadInputTokens"`
		CacheCreate int64 `json:"cacheCreationInputTokens"`
	} `json:"modelUsage"`
}

// reportedTokens converts the Anthropic split, where input_tokens excludes the cached
// part, into [usage.Tokens], where Input includes it.
func reportedTokens(input, output, cacheRead, cacheWrite int64) usage.Tokens {
	r := func(n int64) usage.Count { return usage.Count{N: n, Basis: usage.Reported} }
	return usage.Tokens{Input: r(input + cacheRead), Output: r(output), CacheRead: r(cacheRead), CacheWrite: r(cacheWrite)}
}

// parseClaudeCodeStream reads the JSON lines `claude -p --output-format stream-json`
// prints. A line that is not JSON is skipped: the CLI can print a warning before the
// stream starts. Turns are assistant messages, counted by message id because one
// message with several content blocks arrives as several events.
func parseClaudeCodeStream(out []byte, model string) claudeCodeRun {
	var run claudeCodeRun
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var ev claudeStreamEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			if ev.Message == nil {
				continue
			}
			seen[ev.Message.ID] = true
			if run.firstUseful > 0 {
				continue
			}
			for _, c := range ev.Message.Content {
				if c.Type == "tool_use" && claudeEditTools[c.Name] {
					run.firstUseful = len(seen)
					break
				}
			}
		case "result":
			run.finished, run.isError, run.subtype, run.result = true, ev.IsError, ev.Subtype, ev.Result
			run.turns, run.reportedUSD, run.hasCost = ev.NumTurns, ev.TotalCostUSD, ev.TotalCostUSD > 0
			run.perModel = map[string]usage.Tokens{}
			for name, u := range ev.ModelUsage {
				run.perModel[name] = reportedTokens(u.Input, u.Output, u.CacheRead, u.CacheCreate)
			}
			if len(run.perModel) == 0 && ev.Usage != nil {
				run.perModel[model] = reportedTokens(ev.Usage.Input, ev.Usage.Output, ev.Usage.CacheRead, ev.Usage.CacheCreate)
			}
		}
	}
	if run.turns == 0 {
		run.turns = len(seen)
	}
	return run
}

// codexRun is what a bare `codex exec --json` run reported. Codex names no model calls
// in its stream, so steps counts the items the agent completed (commands, file
// changes, messages), the nearest figure it gives to a turn.
type codexRun struct {
	finished    bool
	failure     string
	lastMessage string
	steps       int
	firstUseful int
	tokens      usage.Tokens
}

type codexStreamEvent struct {
	Type string `json:"type"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage *struct {
		Input  int64 `json:"input_tokens"`
		Cached int64 `json:"cached_input_tokens"`
		Output int64 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// codexStepItems are the completed items counted as a step; reasoning summaries and
// todo lists are not actions.
var codexStepItems = map[string]bool{
	"command_execution": true, "file_change": true, "mcp_tool_call": true, "web_search": true, "agent_message": true,
}

// parseCodexStream reads the JSON lines `codex exec --json` prints. Codex's
// input_tokens already include the cached part, matching [usage.Tokens].
func parseCodexStream(out []byte) codexRun {
	var run codexRun
	r := func(n int64) usage.Count { return usage.Count{N: n, Basis: usage.Reported} }
	run.tokens = usage.Tokens{Input: r(0), Output: r(0), CacheRead: r(0), CacheWrite: r(0)}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var ev codexStreamEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "item.completed":
			if ev.Item == nil || !codexStepItems[ev.Item.Type] {
				continue
			}
			run.steps++
			if ev.Item.Type == "file_change" && run.firstUseful == 0 {
				run.firstUseful = run.steps
			}
			if ev.Item.Type == "agent_message" {
				run.lastMessage = ev.Item.Text
			}
		case "turn.completed":
			run.finished = true
			if ev.Usage != nil {
				run.tokens = run.tokens.Add(usage.Tokens{
					Input: r(ev.Usage.Input), Output: r(ev.Usage.Output), CacheRead: r(ev.Usage.Cached), CacheWrite: r(0),
				})
			}
		case "turn.failed":
			if ev.Error != nil {
				run.failure = ev.Error.Message
			}
		case "error":
			if ev.Message != "" {
				run.failure = strings.TrimSpace(ev.Message)
			}
		}
	}
	return run
}
