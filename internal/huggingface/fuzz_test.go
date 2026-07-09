package huggingface

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

// bodyTransport is a RoundTripper that answers every request with a fixed status
// and body, so a fuzz input drives the client's decode path without a socket.
type bodyTransport struct {
	status int
	body   []byte
}

func (t bodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: t.status,
		Body:       io.NopCloser(bytes.NewReader(t.body)),
		Header:     make(http.Header),
	}, nil
}

// FuzzDecodeResponse drives the three Hub response decoders (Info, Tree, Search)
// from a raw 200 body. Hub JSON is untrusted (a base-URL override can point the
// client at any origin), so the bar is that no body panics or hangs: a malformed
// frame surfaces as a typed error, a well-formed one projects to the client type.
func FuzzDecodeResponse(f *testing.F) {
	seeds := []string{
		`{"id":"org/model","author":"org","tags":["gguf"],"cardData":{"license":"mit"},"gated":false}`,
		`{"id":"m","gated":"manual"}`,
		`{"gated":"false"}`,
		`{"gated":123}`, // gated is neither bool nor string
		`[{"type":"file","path":"a.gguf","size":1,"oid":"x","lfs":{"oid":"y","size":2}}]`,
		`[{"id":"m","downloads":1,"likes":2,"pipeline_tag":"text","library_name":"transformers","tags":["safetensors"]}]`,
		`[]`,
		`{}`,
		`null`,
		`{"tags":123}`, // wrong type for a slice field
		`not json`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(_ *testing.T, body []byte) {
		c := New(WithHTTPClient(&http.Client{Transport: bodyTransport{status: http.StatusOK, body: body}}))
		ctx := context.Background()
		// Each call decodes body into a distinct response type and projects it; the
		// only contract is no panic. Errors are the expected outcome for garbage.
		_, _ = c.Info(ctx, "org/model")
		_, _ = c.Tree(ctx, "org/model")
		_, _ = c.Search(ctx, SearchQuery{Text: "m"})
	})
}
