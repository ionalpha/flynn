package llm_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/llm"
)

func genBlock(rt *rapid.T) llm.Block {
	switch rapid.IntRange(0, 2).Draw(rt, "kind") {
	case 0:
		return llm.Block{Kind: llm.KindText, Text: rapid.String().Draw(rt, "text")}
	case 1:
		return llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{
			ID:    rapid.StringMatching(`[a-z0-9]{1,8}`).Draw(rt, "id"),
			Name:  rapid.StringMatching(`[a-z_]{1,8}`).Draw(rt, "name"),
			Input: json.RawMessage(`{}`),
		}}
	default:
		return llm.Block{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{
			ToolUseID: rapid.StringMatching(`[a-z0-9]{1,8}`).Draw(rt, "useID"),
			Content:   rapid.String().Draw(rt, "content"),
			IsError:   rapid.Bool().Draw(rt, "isErr"),
		}}
	}
}

// TestMessageJSONRoundTripProperty pins that the port's types survive the JSON
// boundary unchanged. They are persisted as goal checkpoints and carried as event
// payloads, so any field that did not round-trip would silently corrupt a resumed
// conversation. For any message, marshal-then-unmarshal must reproduce it exactly,
// and the ToolUses / TextContent projections must stay consistent with the blocks.
func TestMessageJSONRoundTripProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		blocks := rapid.SliceOf(rapid.Custom(genBlock)).Draw(rt, "blocks")
		msg := llm.Message{Role: llm.RoleAssistant, Blocks: blocks}

		b, err := json.Marshal(msg)
		if err != nil {
			rt.Fatalf("marshal: %v", err)
		}
		var got llm.Message
		if err := json.Unmarshal(b, &got); err != nil {
			rt.Fatalf("unmarshal: %v", err)
		}
		if !reflect.DeepEqual(msg, got) {
			rt.Fatalf("round-trip changed the message:\n in: %+v\nout: %+v", msg, got)
		}

		// Projections track the blocks exactly.
		wantUses, wantText := 0, ""
		for _, blk := range blocks {
			switch blk.Kind {
			case llm.KindToolUse:
				wantUses++
			case llm.KindText:
				wantText += blk.Text
			}
		}
		if len(msg.ToolUses()) != wantUses {
			rt.Fatalf("ToolUses() = %d, want %d", len(msg.ToolUses()), wantUses)
		}
		if msg.TextContent() != wantText {
			rt.Fatalf("TextContent() = %q, want %q", msg.TextContent(), wantText)
		}
	})
}
