package llm_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/llm"
)

func genBlock(rt *rapid.T) llm.Block {
	switch rapid.IntRange(0, 3).Draw(rt, "kind") {
	case 0:
		return llm.Block{Kind: llm.KindText, Text: rapid.String().Draw(rt, "text")}
	case 1:
		return llm.Block{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{
			ID:    rapid.StringMatching(`[a-z0-9]{1,8}`).Draw(rt, "id"),
			Name:  rapid.StringMatching(`[a-z_]{1,8}`).Draw(rt, "name"),
			Input: json.RawMessage(`{}`),
		}}
	case 2:
		// Non-empty bytes: an empty []byte marshals to "" and back to a
		// distinct empty slice, which is a JSON-encoding quirk of []byte, not a
		// property of the port worth asserting.
		return llm.ImageBlock(
			rapid.SampledFrom([]string{"image/png", "image/jpeg", "image/gif", "image/webp"}).Draw(rt, "media"),
			rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(rt, "bytes"),
		)
	default:
		return llm.Block{Kind: llm.KindToolResult, ToolResult: &llm.ToolResult{
			ToolUseID: rapid.StringMatching(`[a-z0-9]{1,8}`).Draw(rt, "useID"),
			Content:   rapid.String().Draw(rt, "content"),
			IsError:   rapid.Bool().Draw(rt, "isErr"),
		}}
	}
}

// TestImageBlockCarriesBytesThroughJSON pins that an image's raw bytes and media
// type survive marshal-then-unmarshal, since an image rides the same checkpoint
// persistence as the rest of a conversation and corrupt bytes would reach the model.
func TestImageBlockCarriesBytesThroughJSON(t *testing.T) {
	data := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0x0d, 0x0a}
	blk := llm.ImageBlock("image/png", data)
	if blk.Kind != llm.KindImage || blk.Image == nil {
		t.Fatalf("ImageBlock built %+v, want a KindImage block with an Image", blk)
	}

	b, err := json.Marshal(llm.Message{Role: llm.RoleUser, Blocks: []llm.Block{blk}})
	if err != nil {
		t.Fatal(err)
	}
	var got llm.Message
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	img := got.Blocks[0].Image
	if img == nil || img.MediaType != "image/png" || !bytes.Equal(img.Data, data) {
		t.Fatalf("image did not round-trip: got %+v, want %q %v", img, "image/png", data)
	}
}

func TestSupportedImageMediaType(t *testing.T) {
	for _, mt := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		if !llm.SupportedImageMediaType(mt) {
			t.Errorf("%q should be supported", mt)
		}
	}
	for _, mt := range []string{"image/bmp", "image/tiff", "image/svg+xml", "text/plain", ""} {
		if llm.SupportedImageMediaType(mt) {
			t.Errorf("%q should not be supported", mt)
		}
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
