package localmodel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/bindguard"
	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/internal/inference/launch"
	"github.com/ionalpha/flynn/internal/inference/serve"
	"github.com/ionalpha/flynn/secret"
)

// keyDirName is the directory under the run directory that holds per-launch token files.
const keyDirName = "keys"

// Endpoint is a running model server: where it answers and the token it demands.
type Endpoint struct {
	// ModelID is the catalog id being served.
	ModelID string
	// BaseURL is the OpenAI-compatible endpoint, always on 127.0.0.1.
	BaseURL string
	// Port and PID identify the listener and the server process.
	Port, PID int
	// Token is the bearer token the server requires on every request. It prints as a
	// redaction marker; read it with Expose to build an Authorization header.
	Token secret.Text
	// Reused is true when a server already running was adopted instead of started.
	Reused bool
	// TemplateOverridden is true when the weights embedded a chat template that was
	// replaced with the catalog's.
	TemplateOverridden bool
}

// ServeOptions tunes a launch. The zero value uses the runtime's defaults.
type ServeOptions struct {
	// CtxSize caps the context window; zero keeps the model's default.
	CtxSize int
	// CPUOnly runs the model without GPU offload, the fallback for a model that does not
	// fit in device memory.
	CPUOnly bool
}

// Server is a running server as the registry records it. It carries no token.
type Server struct {
	ModelID   string
	BaseURL   string
	Port      int
	PID       int
	Runtime   string
	StartedAt int64
}

// Stats is a best-effort reading of how loaded a running server is. Known false means
// the load is unknown, not zero.
type Stats = serve.RuntimeStats

// Serve brings a catalog model up and returns its endpoint. In order, it classifies the
// reference and refuses it if the sandbox cannot isolate it, installs the runtime behind
// the advisory gate, fetches the weights against their digest, reads the weights' header
// and replaces any embedded chat template, writes a fresh token to an owner-only file,
// verifies the weights and the runtime binary once more, and starts the server on a
// loopback port inside the sandbox. A server already running for the model is adopted
// when its recorded token file is still readable; otherwise it is stopped and replaced.
//
// The sandbox confines the server's file access but leaves outbound network access open,
// so the runtime could reach the internet. What it cannot do is serve anyone but the
// holder of the token: the listener is loopback-only and the token never appears on the
// command line. After the server answers, Serve sends it a request without the token and
// stops it with [ErrUnauthenticated] if the request is accepted.
func (h *Host) Serve(ctx context.Context, ref string, opts ServeOptions) (Endpoint, error) {
	_, _, a, err := h.assess(ref)
	if err != nil {
		return Endpoint{}, h.fail(err)
	}
	if !a.Admitted {
		return Endpoint{}, h.fail(fmt.Errorf("%w: %s", ErrNotAdmitted, a.Refusal))
	}
	if !a.Catalog {
		return Endpoint{}, h.fail(fmt.Errorf("%w: %s is %s and only catalog models serve", ErrNotServable, a.Ref, a.Trust))
	}
	m, err := h.Model(ref)
	if err != nil {
		return Endpoint{}, h.fail(err)
	}
	if ep, ok, err := h.reuse(ctx, m.ID); err != nil || ok {
		return ep, err
	}
	ep, err := h.start(ctx, m, opts)
	return ep, h.fail(err)
}

func (h *Host) start(ctx context.Context, m Model, opts ServeOptions) (Endpoint, error) {
	q, err := pickQuant(m, "")
	if err != nil {
		return Endpoint{}, err
	}
	rt, err := h.EnsureRuntime(ctx)
	if err != nil {
		return Endpoint{}, err
	}
	w, err := h.weights(ctx, m, q)
	if err != nil {
		return Endpoint{}, err
	}
	// The header parse is the first time the file's contents are interpreted, so the file is
	// frozen and its digest checked first: a parser should not be shown bytes nobody vouched
	// for. Hash and parse share one open file, so the bytes parsed are the bytes hashed.
	if err := os.Chmod(w.Path, 0o400); err != nil {
		return Endpoint{}, fmt.Errorf("localmodel: freeze weights: %w", err)
	}
	dec, err := verifiedTemplate(w, m.ChatTemplate)
	if err != nil {
		return Endpoint{}, err
	}
	port, err := bindguard.FreeLoopbackPort()
	if err != nil {
		return Endpoint{}, err
	}
	token, err := ids.Token()
	if err != nil {
		return Endpoint{}, err
	}
	keyPath := h.keyPath(m.ID)
	plan, err := launch.BuildPlan(launch.Config{
		BinPath: rt.BinPath, WeightsPath: w.Path, Model: m, Format: q.Format, Port: port,
		CtxSize: opts.CtxSize, CPUOnly: opts.CPUOnly, APIKeyFile: keyPath, ModelEmbedsTemplate: dec.ModelSupplied,
	})
	if err != nil {
		return Endpoint{}, err
	}
	// The last check before launch, run after everything slow so the window between it and
	// the runtime opening the file is as short as it can be made. The weights are already
	// read-only, so only the owning account can write into that window.
	if err := verifyWeights(w); err != nil {
		return Endpoint{}, err
	}
	if err := verifyRuntimeBinary(rt.BinPath); err != nil {
		return Endpoint{}, err
	}
	if err := fsatomic.WriteFile(keyPath, []byte(token), 0o600); err != nil {
		return Endpoint{}, fmt.Errorf("localmodel: write token file: %w", err)
	}
	h.remember(port, token)
	sep, err := h.mgr.Ensure(ctx, serve.EnsureConfig{ModelID: m.ID, Runtime: Runtime, Plan: plan, Confine: true})
	if err != nil {
		h.forgetPort(port)
		return Endpoint{}, err
	}
	if sep.Reused {
		// Another launch won the race. The token file just written belongs to a server
		// that is not running, so the winner cannot be trusted to match it.
		h.forgetPort(port)
		_, _ = h.mgr.Stop(m.ID)
		return Endpoint{}, errors.New("localmodel: a server for this model appeared during launch; retry")
	}
	if err := h.requireToken(ctx, sep.BaseURL); err != nil {
		_, _ = h.mgr.Stop(m.ID)
		h.forgetPort(port)
		return Endpoint{}, err
	}
	return Endpoint{
		ModelID: m.ID, BaseURL: sep.BaseURL, Port: sep.Port, PID: sep.PID,
		Token: secret.New(token), TemplateOverridden: plan.TemplateOverridden,
	}, nil
}

// reuse adopts a running server for the model. A server whose token file cannot be read
// is stopped, because an endpoint nobody holds the token for is of no use and one that
// might be someone else's should not be left up.
func (h *Host) reuse(ctx context.Context, id string) (Endpoint, bool, error) {
	recs, err := h.mgr.Status(ctx)
	if err != nil {
		return Endpoint{}, false, h.fail(err)
	}
	for _, r := range recs {
		if r.ModelID != id || r.ContainerID != "" {
			continue
		}
		tok, ok := h.readToken(id)
		if !ok {
			_, _ = h.mgr.Stop(id)
			return Endpoint{}, false, nil
		}
		h.remember(r.Port, tok)
		if err := h.requireToken(ctx, r.BaseURL); err != nil {
			_, _ = h.mgr.Stop(id)
			h.forgetPort(r.Port)
			return Endpoint{}, false, h.fail(err)
		}
		return Endpoint{ModelID: id, BaseURL: r.BaseURL, Port: r.Port, PID: r.PID, Token: secret.New(tok), Reused: true}, true, nil
	}
	return Endpoint{}, false, nil
}

// Reach returns the endpoint of a model already running, token included, or
// [ErrNotRunning]. It checks the token requirement again before handing the endpoint out.
func (h *Host) Reach(ctx context.Context, id string) (Endpoint, error) {
	ep, ok, err := h.reuse(ctx, id)
	switch {
	case err != nil:
		return Endpoint{}, err
	case !ok:
		return Endpoint{}, fmt.Errorf("%w: %s", ErrNotRunning, id)
	}
	return ep, nil
}

// Status lists the servers that currently answer. A record whose server has stopped
// answering is pruned and its process reclaimed.
func (h *Host) Status(ctx context.Context) ([]Server, error) {
	recs, err := h.mgr.Status(ctx)
	if err != nil {
		return nil, h.fail(err)
	}
	out := make([]Server, 0, len(recs))
	for _, r := range recs {
		out = append(out, Server{ModelID: r.ModelID, BaseURL: r.BaseURL, Port: r.Port, PID: r.PID, Runtime: r.Runtime, StartedAt: r.StartedAt})
	}
	return out, nil
}

// Stop ends the server for a model and deletes its token file. It reports whether a
// server was found.
func (h *Host) Stop(id string) (bool, error) {
	if recs, err := h.mgr.Status(context.Background()); err == nil {
		for _, r := range recs {
			if r.ModelID == id {
				h.forgetPort(r.Port)
			}
		}
	}
	found, err := h.mgr.Stop(id)
	_ = os.Remove(h.keyPath(id))
	return found, h.fail(err)
}

// Stats reads the load of a running server. It returns [ErrNotRunning] when there is
// none, and an unknown reading, not an error, when the server cannot be read.
func (h *Host) Stats(ctx context.Context, id string) (Stats, error) {
	if _, err := h.Reach(ctx, id); err != nil {
		return Stats{}, err
	}
	s, err := h.mgr.Stats(ctx, id)
	return s, h.fail(err)
}

func (h *Host) keyPath(id string) string {
	return filepath.Join(h.dataDir, "run", keyDirName, keyFileName(id))
}

func (h *Host) remember(port int, token string) {
	h.mu.Lock()
	h.tokens[port] = token
	h.mu.Unlock()
}

func (h *Host) forgetPort(port int) {
	h.mu.Lock()
	delete(h.tokens, port)
	h.mu.Unlock()
}

// readToken returns the token a launch recorded for the model, if the file is still
// readable and holds one.
func (h *Host) readToken(id string) (string, bool) {
	b, err := os.ReadFile(h.keyPath(id))
	tok := strings.TrimSpace(string(b))
	return tok, err == nil && tok != ""
}

// requireToken sends the server a chat request with no token and returns
// [ErrUnauthenticated] unless it is refused with 401 or 403. Anything else, including a
// failure to ask, counts as not enforced: a server that cannot be shown to require the
// token is not handed out.
func (h *Host) requireToken(ctx context.Context, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", strings.NewReader(`{"model":"probe","messages":[]}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client(false).Do(req)
	if err != nil {
		return fmt.Errorf("%w: could not check: %w", ErrUnauthenticated, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return fmt.Errorf("%w: status %d", ErrUnauthenticated, resp.StatusCode)
	}
	return nil
}

// Client returns an HTTP client for the servers this host manages. It reaches only
// 127.0.0.1, follows no redirect, uses no proxy, and attaches the token of whichever
// server the request addresses, found from the run directory when this process did not
// start it. It has no overall timeout; bound a call with its context. A request to any
// other address fails before it is sent, so the token cannot be carried off the machine.
func (h *Host) Client() *http.Client { return h.newClient(true, 0) }

// loopbackClient is the client the manager probes and reads metrics with.
func (h *Host) loopbackClient() *http.Client { return h.newClient(true, 5*time.Second) }

// client builds a short-timeout loopback client. withToken attaches the recorded token.
func (h *Host) client(withToken bool) *http.Client { return h.newClient(withToken, 5*time.Second) }

// newClient builds a loopback HTTP client. Redirects are never followed and proxies never
// used, so a token cannot be carried to a host the server names.
func (h *Host) newClient(withToken bool, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     &loopbackTransport{h: h, withToken: withToken, base: &http.Transport{Proxy: nil}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// tokenFor returns the token of the server listening on port: the one this process
// remembered, or else the one the registry's record for that port points at.
func (h *Host) tokenFor(port int) string {
	h.mu.Lock()
	tok := h.tokens[port]
	h.mu.Unlock()
	if tok != "" {
		return tok
	}
	recs, err := h.reg.List()
	if err != nil {
		return ""
	}
	for _, r := range recs {
		if r.Port != port || r.ContainerID != "" {
			continue
		}
		if tok, ok := h.readToken(r.ModelID); ok {
			h.remember(port, tok)
			return tok
		}
	}
	return ""
}

type loopbackTransport struct {
	h         *Host
	withToken bool
	base      http.RoundTripper
}

func (t *loopbackTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() != "127.0.0.1" {
		return nil, errors.New("localmodel: refusing to send a request off loopback")
	}
	if t.withToken {
		port, err := strconv.Atoi(r.URL.Port())
		if err == nil {
			if tok := t.h.tokenFor(port); tok != "" {
				r = r.Clone(r.Context())
				r.Header.Set("Authorization", "Bearer "+tok)
			}
		}
	}
	return t.base.RoundTrip(r)
}
