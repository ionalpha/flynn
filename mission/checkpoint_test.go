package mission

import (
	"bytes"
	"reflect"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
)

// countAssistant counts the assistant messages in a transcript, one per model turn.
func countAssistant(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant {
			n++
		}
	}
	return n
}

// genMessages builds a random append-only transcript: assistant turns (a tool call
// or final text) interleaved with the user messages that answer them.
func genMessages(t *rapid.T) []llm.Message {
	n := rapid.IntRange(0, 12).Draw(t, "messages")
	msgs := make([]llm.Message, 0, n)
	for range n {
		id := rapid.StringMatching(`t[0-9]`).Draw(t, "id")
		switch rapid.IntRange(0, 3).Draw(t, "kind") {
		case 0:
			msgs = append(msgs, callMsg(id, "read"))
		case 1:
			msgs = append(msgs, resultMsg(id, rapid.String().Draw(t, "content"), rapid.Bool().Draw(t, "err")))
		case 2:
			msgs = append(msgs, llm.Text(llm.RoleAssistant, rapid.String().Draw(t, "say")))
		default:
			msgs = append(msgs, llm.Text(llm.RoleUser, rapid.String().Draw(t, "ask")))
		}
	}
	return msgs
}

// TestCheckpointRoundTrip proves the stored form carries every field back losslessly,
// including the fan-out pending slots and the carried turn count.
func TestCheckpointRoundTrip(t *testing.T) {
	cp := checkpoint{
		Messages: []llm.Message{
			llm.Text(llm.RoleUser, "do the thing"),
			callMsg("c1", "read"),
			resultMsg("c1", "big output", false),
			llm.Text(llm.RoleAssistant, "done"),
		},
		Done:       true,
		Result:     "done",
		VerifyUsed: 2,
		Turns:      2,
		Pending:    []resultSlot{{ChildID: "child-1", ToolUseID: "u1", Content: "pending", IsError: false}},
	}
	raw, err := encodeCheckpoint(cp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeCheckpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Messages, cp.Messages) {
		t.Fatalf("messages round-trip mismatch:\n got %+v\nwant %+v", got.Messages, cp.Messages)
	}
	if got.Done != cp.Done || got.Result != cp.Result || got.VerifyUsed != cp.VerifyUsed || got.Turns != cp.Turns {
		t.Fatalf("scalars round-trip mismatch: %+v", got)
	}
	if !reflect.DeepEqual(got.Pending, cp.Pending) {
		t.Fatalf("pending round-trip mismatch: %+v", got.Pending)
	}
}

// TestCheckpointDeltaMatchesFull is the correctness backbone of the delta write: the
// cache is a pure optimization, so encoding a checkpoint whose prefix was decoded (a
// warm cache) and then extended by a turn must yield bytes identical to marshaling
// the whole transcript from scratch. That keeps the stored form replay-equivalent
// regardless of how the history was assembled.
func TestCheckpointDeltaMatchesFull(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		msgs := genMessages(t)
		full := checkpoint{Messages: msgs, Turns: countAssistant(msgs)}

		b1, err := encodeCheckpoint(full) // cold cache: every message marshaled here
		if err != nil {
			t.Fatal(err)
		}
		dec, err := decodeCheckpoint(b1) // warm cache: prefix bytes retained
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(dec.Messages, msgs) {
			t.Fatalf("decode mismatch:\n got %+v\nwant %+v", dec.Messages, msgs)
		}
		if b2, err := encodeCheckpoint(dec); err != nil {
			t.Fatal(err)
		} else if !bytes.Equal(b1, b2) {
			t.Fatalf("warm-cache re-encode differs from cold:\n cold %s\n warm %s", b1, b2)
		}

		// Append a turn onto the warm checkpoint and re-encode: the appended message
		// has no cached bytes and is marshaled fresh, the prefix is copied. The result
		// must equal a full marshal of the extended transcript.
		extra := llm.Text(llm.RoleAssistant, rapid.String().Draw(t, "extra"))
		dec.Messages = append(dec.Messages, extra)
		dec.Turns++
		delta, err := encodeCheckpoint(dec)
		if err != nil {
			t.Fatal(err)
		}
		fresh := checkpoint{Messages: append(append([]llm.Message{}, msgs...), extra), Turns: countAssistant(msgs) + 1}
		fullAgain, err := encodeCheckpoint(fresh)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(delta, fullAgain) {
			t.Fatalf("delta encode differs from full:\n delta %s\n full  %s", delta, fullAgain)
		}
	})
}

// TestCheckpointTurnsInvariant proves the carried turn count equals the number of
// assistant messages across the operations that mutate a checkpoint: an assistant
// turn increments it, while folding a fan-out result and continuing the conversation
// (both append a user message) leave it untouched. The count is telemetry, so it must
// not drift from the history it describes.
func TestCheckpointTurnsInvariant(t *testing.T) {
	// A fan-out fold appends a user message (the children's results). Turns unchanged.
	cp := checkpoint{
		Messages: []llm.Message{llm.Text(llm.RoleUser, "go"), callMsg("c1", "spawn")}, // one assistant turn: the spawn call
		Turns:    1,
	}
	cp.Messages = append(cp.Messages, resultMsg("c1", "child done", false)) // the fold
	if cp.Turns != countAssistant(cp.Messages) {
		t.Fatalf("fan-out fold drifted Turns: have %d, assistant messages %d", cp.Turns, countAssistant(cp.Messages))
	}

	// ContinueConversation reopens a converged goal with a new user line. Turns unchanged.
	cp.Done = true
	raw, err := encodeCheckpoint(cp)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ContinueConversation(goal.Status{Checkpoint: raw}, "another ask")
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeCheckpoint(next.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got.Turns != 1 || got.Turns != countAssistant(got.Messages) {
		t.Fatalf("ContinueConversation drifted Turns: have %d, assistant messages %d", got.Turns, countAssistant(got.Messages))
	}
}
