package hybrid_test

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// concepts is the fake embedding space: one dimension per topic, and the words
// that mean that topic. It is what makes these tests about retrieval rather than
// about a model. A real embedder connects "datastore" to "database" because it was
// trained to; this one does it because the table says so, and the recall behaviour
// under test is identical either way.
var concepts = [][]string{
	{"database", "db", "datastore", "postgres", "sqlite", "storage", "engine"},
	{"choice", "choose", "chose", "pick", "picked", "settled", "decided", "decision"},
	{"release", "ship", "shipped", "deploy", "launch", "rollout"},
	{"thursday", "afternoon", "window", "when", "schedule", "time"},
	{"tabs", "spaces", "indent", "editor", "formatting", "style"},
	{"prefers", "likes", "wants", "preference", "favourite"},
	{"retry", "retries", "flaky", "timeout", "failure", "failing"},
}

// fakeEmbedder scores text by how many words it has from each concept, which is a
// bag-of-concepts vector: two texts about one topic point the same way whether or
// not they share a word. It counts its calls and the texts it was asked for, so a
// test can assert the cache is doing its job.
type fakeEmbedder struct {
	mu    sync.Mutex
	calls int
	texts int
	err   error
	// short returns one fewer vector than asked for, the contract violation a host
	// embedder can commit.
	short bool
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls++
	f.texts += len(texts)
	err, short := f.err, f.short
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([][]float32, 0, len(texts))
	for _, t := range texts {
		out = append(out, vectorFor(t))
	}
	if short && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out, nil
}

func (f *fakeEmbedder) counts() (calls, texts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.texts
}

var errEmbedder = errors.New("model unavailable")

func vectorFor(text string) []float32 {
	vec := make([]float32, len(concepts))
	for _, word := range strings.Fields(strings.ToLower(text)) {
		word = strings.Trim(word, ".,:;?!\"'()")
		for i, members := range concepts {
			for _, m := range members {
				if word == m {
					vec[i]++
				}
			}
		}
	}
	return vec
}
