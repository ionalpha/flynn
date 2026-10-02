package embed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/llm/embed"
	"github.com/ionalpha/flynn/secret"
)

// echoTransport answers each batch by embedding every input as a vector carrying the
// text's own length, and returns the entries in reverse order. Reversing is the
// point: a client that trusted arrival order would pass this test on a batch of one
// and fail it on every larger batch, which is exactly the bug that never shows up in
// a small example.
type echoTransport struct{ seen []string }

func (e *echoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var in struct {
		Input []string `json:"input"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &in)
	e.seen = append(e.seen, in.Input...)

	var entries []string
	for i := len(in.Input) - 1; i >= 0; i-- {
		entries = append(entries, fmt.Sprintf(`{"index":%d,"embedding":[%d]}`, i, len(in.Input[i])))
	}
	reply := `{"data":[` + strings.Join(entries, ",") + `]}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(reply)), Header: make(http.Header)}, nil
}

// TestEmbedPreservesTheCorpusUnderAnyBatching is the property the whole adapter
// exists to hold: however a corpus is split into requests and however an endpoint
// orders its replies, the caller gets one vector per text, in the order it asked,
// and the endpoint sees every text once in that same order.
//
// It is the property because both halves of the ranking downstream depend on it. A
// vector paired with the wrong text ranks a memory by another memory's meaning, and
// nothing in a recall can detect that: the results look plausible and are wrong.
func TestEmbedPreservesTheCorpusUnderAnyBatching(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		texts := rapid.SliceOfN(rapid.StringMatching(`[a-z]{1,12}`), 1, 40).Draw(t, "texts")
		batch := rapid.IntRange(1, 50).Draw(t, "batch")

		tr := &echoTransport{}
		c := embed.New(secret.New("test-key"),
			embed.WithHTTPClient(&http.Client{Transport: tr}),
			embed.WithBatch(batch),
		)

		got, err := c.Embed(context.Background(), texts)
		if err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if len(got) != len(texts) {
			t.Fatalf("vectors = %d, want one per text (%d)", len(got), len(texts))
		}
		for i, v := range got {
			if len(v) != 1 || int(v[0]) != len(texts[i]) {
				t.Fatalf("vector %d = %v, want the one belonging to %q", i, v, texts[i])
			}
		}
		if strings.Join(tr.seen, "\x00") != strings.Join(texts, "\x00") {
			t.Fatalf("the endpoint saw %v, want the corpus once and in order", tr.seen)
		}
	})
}
