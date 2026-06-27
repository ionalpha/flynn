package openai

import (
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/secret"
)

func TestBuildRequestSampling(t *testing.T) {
	c := New(secret.New("test-key"), WithModel("m"))
	msg := []llm.Message{llm.Text(llm.RoleUser, "hi")}

	// Free-running: a request with no sampling sends no sampler fields.
	r, _ := c.buildRequest(llm.Request{Messages: msg})
	if r.Temperature != nil || r.TopP != nil || r.Seed != nil {
		t.Fatalf("a free-running request must not pin sampling: %+v", r)
	}

	// Pinned greedy decoding with a seed and a top-p: all three are sent.
	r, _ = c.buildRequest(llm.Request{Messages: msg, Sampling: &llm.Sampling{Seed: 42, Temperature: 0, TopP: 0.9}})
	if r.Temperature == nil || *r.Temperature != 0 {
		t.Fatalf("greedy temperature 0 must be sent, got %v", r.Temperature)
	}
	if r.Seed == nil || *r.Seed != 42 {
		t.Fatalf("seed must be sent, got %v", r.Seed)
	}
	if r.TopP == nil || *r.TopP != 0.9 {
		t.Fatalf("top-p must be sent, got %v", r.TopP)
	}

	// A degenerate top-p of zero is omitted, while temperature and seed still send.
	r, _ = c.buildRequest(llm.Request{Messages: msg, Sampling: &llm.Sampling{Seed: 1, Temperature: 0.5, TopP: 0}})
	if r.TopP != nil {
		t.Fatalf("a zero top-p must be omitted, got %v", *r.TopP)
	}
	if r.Temperature == nil || r.Seed == nil {
		t.Fatal("temperature and seed must still be sent when top-p is omitted")
	}

	// Out-of-range sampling is normalized before it is sent.
	r, _ = c.buildRequest(llm.Request{Messages: msg, Sampling: &llm.Sampling{Temperature: -3, TopP: 9}})
	if r.Temperature == nil || *r.Temperature != 0 {
		t.Fatalf("negative temperature must normalize to 0, got %v", r.Temperature)
	}
	if r.TopP == nil || *r.TopP != 1 {
		t.Fatalf("top-p above 1 must normalize to 1, got %v", r.TopP)
	}
}
