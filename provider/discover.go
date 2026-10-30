package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/secret"
)

// LocalModel is a model a running local server reports it already has. It comes from
// the server's own answer, not from Flynn's catalog, so nothing about it is verified:
// the size and the tool capability are what the server says.
type LocalModel struct {
	// Server is the provider kind that serves it: ollama or lmstudio.
	Server string
	// ID is the model name the server expects in a request.
	ID string
	// Params is the parameter count as the server words it (such as "20.9B"); empty
	// when the server does not report one.
	Params string
	// Quant is the quantization level the server reports; empty when it does not.
	Quant string
	// Tools reports whether the server lists tool calling among the model's
	// capabilities. Only Ollama reports capabilities; for LM Studio it is false and
	// means unknown.
	Tools bool
}

// Spec is the --model value that runs it.
func (m LocalModel) Spec() string { return m.Server + ":" + m.ID }

const (
	// discoverTimeout bounds one probe. A server on the loopback host answers in
	// milliseconds; a long wait would be a stalled or non-server port, and
	// discovery runs in a listing command that must not hang on it.
	discoverTimeout = 750 * time.Millisecond
	// discoverMaxBody caps what is read from a server, so a port that answers with
	// something other than a model list cannot fill memory.
	discoverMaxBody = 4 << 20
	// discoverMaxModels caps the per-model /api/show calls, one HTTP request each.
	discoverMaxModels = 64
)

// DiscoverLocal asks the local servers Flynn knows how to ask which models they
// already have: Ollama through /api/tags and /api/show, LM Studio through
// /v1/models. Each server is probed at the address a run would use (its default or
// its *_BASE_URL reference in src), and only a base URL that is safe to send to is
// probed. A server that is not running, answers with an error or answers with
// something unexpected contributes no models; absence is the normal case, so it is
// not an error. A nil hc uses a client with a short timeout.
func DiscoverLocal(ctx context.Context, src secret.Source, hc *http.Client) []LocalModel {
	if hc == nil {
		hc = &http.Client{Timeout: discoverTimeout}
	}
	var out []LocalModel
	if base, ok := localBase(ctx, src, "ollama"); ok {
		out = append(out, discoverOllama(ctx, hc, base)...)
	}
	if base, ok := localBase(ctx, src, "lmstudio"); ok {
		out = append(out, discoverLMStudio(ctx, hc, base)...)
	}
	return out
}

func localBase(ctx context.Context, src secret.Source, kind string) (string, bool) {
	k, _ := localKindNamed(kind)
	base := k.defaultBaseURL
	if u, err := src.Lookup(ctx, k.baseRef); err == nil && u.Expose() != "" {
		base = u.Expose()
	}
	if !llm.SafeBaseURL(base) {
		return "", false
	}
	return strings.TrimRight(base, "/"), true
}

func getJSON(ctx context.Context, hc *http.Client, url string, v any) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	return doJSON(hc, req, v)
}

func postJSON(ctx context.Context, hc *http.Client, url string, body, v any) bool {
	raw, err := json.Marshal(body)
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON(hc, req, v)
}

func doJSON(hc *http.Client, req *http.Request, v any) bool {
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(io.LimitReader(resp.Body, discoverMaxBody)).Decode(v) == nil
}

// discoverOllama lists /api/tags and enriches each model from /api/show. Ollama's
// native API sits beside its OpenAI-compatible /v1 root, so the root's /v1 suffix is
// dropped to reach it.
func discoverOllama(ctx context.Context, hc *http.Client, base string) []LocalModel {
	root := strings.TrimSuffix(base, "/v1")
	var tags struct {
		Models []struct {
			Name    string `json:"name"`
			Details struct {
				ParameterSize     string `json:"parameter_size"`
				QuantizationLevel string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if !getJSON(ctx, hc, root+"/api/tags", &tags) {
		return nil
	}
	var out []LocalModel
	for i, t := range tags.Models {
		if t.Name == "" {
			continue
		}
		m := LocalModel{Server: "ollama", ID: t.Name, Params: t.Details.ParameterSize, Quant: t.Details.QuantizationLevel}
		if i < discoverMaxModels {
			var show struct {
				Details struct {
					ParameterSize     string `json:"parameter_size"`
					QuantizationLevel string `json:"quantization_level"`
				} `json:"details"`
				Capabilities []string `json:"capabilities"`
			}
			if postJSON(ctx, hc, root+"/api/show", map[string]string{"model": t.Name}, &show) {
				if m.Params == "" {
					m.Params = show.Details.ParameterSize
				}
				if m.Quant == "" {
					m.Quant = show.Details.QuantizationLevel
				}
				for _, c := range show.Capabilities {
					if c == "tools" {
						m.Tools = true
					}
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// discoverLMStudio lists /v1/models. LM Studio returns embedding models in the same
// list; they are kept, because the listing reports what the server has and a caller
// that wants chat models filters by what it knows of the id.
func discoverLMStudio(ctx context.Context, hc *http.Client, base string) []LocalModel {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if !getJSON(ctx, hc, base+"/models", &list) {
		return nil
	}
	var out []LocalModel
	for _, d := range list.Data {
		if d.ID != "" {
			out = append(out, LocalModel{Server: "lmstudio", ID: d.ID})
		}
	}
	return out
}

// String renders a model for a listing: its spec, with the size and quantization the
// server reported.
func (m LocalModel) String() string {
	var extra []string
	if m.Params != "" {
		extra = append(extra, m.Params)
	}
	if m.Quant != "" {
		extra = append(extra, m.Quant)
	}
	if len(extra) == 0 {
		return m.Spec()
	}
	return fmt.Sprintf("%s (%s)", m.Spec(), strings.Join(extra, ", "))
}
