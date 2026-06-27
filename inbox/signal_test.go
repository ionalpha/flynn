package inbox_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/resource"
)

// newStore returns an in-memory resource store with the Signal kind registered.
func newStore(t *testing.T) resource.Store {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := inbox.RegisterKind(reg); err != nil {
		t.Fatal(err)
	}
	return resource.NewMemory(reg)
}

// TestSignalIsAFirstClassResource proves a Signal is stored and read back through
// the generic resource store like any other kind, with its spec intact.
func TestSignalIsAFirstClassResource(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	spec := inbox.Spec{
		Source:       "telegram",
		Conversation: "c1",
		Sender:       "ada",
		Type:         "message",
		Content:      "hello",
		Metadata:     map[string]string{"chat": "4242"},
		ReceivedAt:   time.Unix(1_700_000_000, 0).UTC(),
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Put(ctx, resource.Resource{
		APIVersion:   inbox.GroupVersion,
		Kind:         inbox.Kind,
		GenerateName: "sig-",
		Spec:         raw,
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := s.Get(ctx, inbox.Kind, resource.Scope{}, saved.Name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	gotSpec, err := inbox.DecodeSpec(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotSpec.Source != spec.Source || gotSpec.Content != spec.Content ||
		gotSpec.Conversation != spec.Conversation || gotSpec.Sender != spec.Sender ||
		gotSpec.Metadata["chat"] != "4242" || !gotSpec.ReceivedAt.Equal(spec.ReceivedAt) {
		t.Fatalf("round-trip spec = %+v, want %+v", gotSpec, spec)
	}
}

// TestPutRejectsSignalWithoutSource confirms the kind's schema is enforced: a
// signal must name the source it arrived on (the reply-routing key).
func TestPutRejectsSignalWithoutSource(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	raw, _ := json.Marshal(inbox.Spec{Content: "orphan"})
	if _, err := s.Put(ctx, resource.Resource{APIVersion: inbox.GroupVersion, Kind: inbox.Kind, GenerateName: "sig-", Spec: raw}); err == nil {
		t.Fatal("put with no source = nil error, want schema rejection")
	}
}

// TestSpecCodecProperty is the rigor property: decoding an encoded spec and
// re-encoding it yields identical bytes, so the envelope is a lossless record of
// any inbound content (any source, sender, unicode body, metadata, or time).
func TestSpecCodecProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		spec := drawSpec(rt)
		raw, err := json.Marshal(spec)
		if err != nil {
			rt.Fatalf("marshal: %v", err)
		}
		decoded, err := inbox.DecodeSpec(resource.Resource{Spec: raw})
		if err != nil {
			rt.Fatalf("decode: %v", err)
		}
		raw2, err := json.Marshal(decoded)
		if err != nil {
			rt.Fatalf("re-marshal: %v", err)
		}
		if string(raw) != string(raw2) {
			rt.Fatalf("codec not a fixed point:\n %s\n %s", raw, raw2)
		}
	})
}

// TestStatusCodecProperty is the same lossless property for the triage status.
func TestStatusCodecProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		st := drawStatus(rt)
		raw, err := st.Encode()
		if err != nil {
			rt.Fatalf("encode: %v", err)
		}
		decoded, err := inbox.DecodeStatus(resource.Resource{Status: raw})
		if err != nil {
			rt.Fatalf("decode: %v", err)
		}
		raw2, err := decoded.Encode()
		if err != nil {
			rt.Fatalf("re-encode: %v", err)
		}
		if string(raw) != string(raw2) {
			rt.Fatalf("status codec not a fixed point:\n %s\n %s", raw, raw2)
		}
	})
}

func drawSpec(rt *rapid.T) inbox.Spec {
	return inbox.Spec{
		Source:       rapid.String().Draw(rt, "source"),
		Conversation: rapid.String().Draw(rt, "conversation"),
		Sender:       rapid.String().Draw(rt, "sender"),
		Type:         rapid.String().Draw(rt, "type"),
		Content:      rapid.String().Draw(rt, "content"),
		Metadata:     rapid.MapOfN(rapid.String(), rapid.String(), 0, 4).Draw(rt, "metadata"),
		ReceivedAt:   time.Unix(rapid.Int64Range(0, 4_102_444_800).Draw(rt, "receivedAt"), 0).UTC(),
	}
}

func drawStatus(rt *rapid.T) inbox.Status {
	dispositions := []inbox.Disposition{
		"", inbox.DispositionReply, inbox.DispositionGoal,
		inbox.DispositionStore, inbox.DispositionNotify, inbox.DispositionDrop,
	}
	phases := []inbox.Phase{"", inbox.PhaseReceived, inbox.PhaseTriaged, inbox.PhaseActed, inbox.PhaseDropped}
	return inbox.Status{
		Phase:            rapid.SampledFrom(phases).Draw(rt, "phase"),
		Disposition:      rapid.SampledFrom(dispositions).Draw(rt, "disposition"),
		ObservedSpecHash: rapid.String().Draw(rt, "hash"),
		GoalName:         rapid.String().Draw(rt, "goalName"),
		Message:          rapid.String().Draw(rt, "message"),
	}
}
