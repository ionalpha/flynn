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
	"github.com/ionalpha/flynn/internal/inference"
	"github.com/ionalpha/flynn/internal/inference/launch"
	"github.com/ionalpha/flynn/internal/inference/serve"
	"github.com/ionalpha/flynn/sandbox"
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
	// Containment names the isolation the server runs under: "kernel-confined" or
	// "process-jail". It is what the sandbox reported when the server was started, not
	// what was requested.
	Containment string
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

// Serve brings a catalog model up and returns its endpoint. Calls for one model run one
// at a time, so a server is never started twice or stopped by a racing call. A server
// already running is adopted only if it was started from a build at or above the current
// floor, under at least the isolation [Config.MinContainment] asks for, with a key file
// still readable, and if its listener passes the handshake below; otherwise it is stopped
// and replaced. A launch installs the runtime behind the advisory gate, fetches the weights
// against their digest, replaces any embedded chat template, and checks the weights and the
// runtime binary once more before starting the server on a loopback port.
//
// Another process can take the chosen port before the runtime binds it, so the token goes
// nowhere until the listener is attributed: Serve sends it a request with no credential and
// one with a made-up one, both of which it must refuse, then one with a probe key, which it
// must not refuse. Failing the first two returns [ErrUnauthenticated], the third
// [ErrWrongListener]; the server is stopped either way.
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
	unlock, err := h.locks.lock(ctx, m.ID)
	if err != nil {
		return Endpoint{}, err
	}
	defer unlock()
	if ep, ok, err := h.reuse(ctx, m.ID); err != nil || ok {
		return ep, err
	}
	have := h.containment()
	if have < h.minContainment {
		return Endpoint{}, h.fail(fmt.Errorf("%w: a model server needs %s confinement, but this host provides %s; set Config.MinContainment to \"process\" to accept a process jail", ErrNotAdmitted, h.minContainment, have))
	}
	ep, err := h.start(ctx, m, opts, have)
	return ep, h.fail(err)
}

func (h *Host) start(ctx context.Context, m Model, opts ServeOptions, have sandbox.Containment) (Endpoint, error) {
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
	probeKey, err := ids.Token()
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
	// read-only, so only the owning account can write into that window. The hook lets a
	// test change the file, or take the port, inside it.
	if h.preLaunch != nil {
		h.preLaunch(port)
	}
	if err := verifyWeights(w); err != nil {
		return Endpoint{}, err
	}
	if err := verifyRuntimeBinary(rt.BinPath); err != nil {
		return Endpoint{}, err
	}
	if err := fsatomic.WriteFile(keyPath, []byte(token+"\n"+probeKey+"\n"), 0o600); err != nil {
		return Endpoint{}, fmt.Errorf("localmodel: write token file: %w", err)
	}
	sep, err := h.mgr.Ensure(ctx, serve.EnsureConfig{
		ModelID: m.ID, Runtime: Runtime, Plan: plan, Confine: true,
		Version: rt.Version, Containment: have.String(),
		Attest: func(ctx context.Context, baseURL string) error { return h.attest(ctx, baseURL, probeKey) },
	})
	if err != nil {
		_ = os.Remove(keyPath)
		return Endpoint{}, err
	}
	if sep.Reused {
		// Another process won the race. The key file just written belongs to a server
		// that is not running, so the winner cannot be trusted to match it.
		_, _ = h.mgr.Stop(m.ID)
		_ = os.Remove(keyPath)
		return Endpoint{}, errors.New("localmodel: a server for this model appeared during launch; retry")
	}
	// The token is remembered only now: until the listener was attributed, a request made
	// through [Host.Client] to this port would have carried it.
	h.remember(sep.Port, token)
	return Endpoint{
		ModelID: m.ID, BaseURL: sep.BaseURL, Port: sep.Port, PID: sep.PID,
		Token: secret.New(token), TemplateOverridden: plan.TemplateOverridden,
		Containment: have.String(),
	}, nil
}

// reuse adopts a running server for the model, or stops it and reports none. A server is
// stopped, and its key file removed, when it was started from a build below the floor now
// in force (or from one the record does not name), when it runs under less isolation than
// [Config.MinContainment] asks for, or when its key file cannot be read: an endpoint
// nobody holds the token for is of no use, and one that might be someone else's should not
// be left up. A server that passes those is attested before its token is remembered or
// returned, and one that fails attestation is stopped with the error.
func (h *Host) reuse(ctx context.Context, id string) (Endpoint, bool, error) {
	recs, err := h.mgr.Status(ctx)
	if err != nil {
		return Endpoint{}, false, h.fail(err)
	}
	for _, r := range recs {
		if r.ModelID != id || r.ContainerID != "" {
			continue
		}
		keys := h.readKeys(id)
		if !h.adoptable(r) || len(keys) < 2 {
			h.dropServer(id, r.Port)
			return Endpoint{}, false, nil
		}
		if err := h.attest(ctx, r.BaseURL, keys[1]); err != nil {
			h.dropServer(id, r.Port)
			return Endpoint{}, false, h.fail(err)
		}
		h.remember(r.Port, keys[0])
		return Endpoint{
			ModelID: id, BaseURL: r.BaseURL, Port: r.Port, PID: r.PID, Token: secret.New(keys[0]),
			Reused: true, Containment: containmentOf(r.Containment).String(),
		}, true, nil
	}
	return Endpoint{}, false, nil
}

// adoptable reports whether a recorded server may be handed out: its build passes the
// floor in force now, and its isolation meets [Config.MinContainment]. A record that
// names no build or no isolation was written by a launch that could not say, and is held
// to the strictest reading.
func (h *Host) adoptable(r serve.Record) bool {
	v := inference.ParseVersion(r.Version)
	if len(v) == 0 || h.gate(v) != nil {
		return false
	}
	return containmentOf(r.Containment) >= h.minContainment
}

// containmentOf reads the isolation level a record names. Anything but the kernel level,
// including a record that names none, is the process jail.
func containmentOf(s string) sandbox.Containment {
	if s == sandbox.ContainmentKernel.String() {
		return sandbox.ContainmentKernel
	}
	return sandbox.ContainmentNone
}

// dropServer stops the server for a model, forgets its port and deletes its key file.
func (h *Host) dropServer(id string, port int) {
	h.forgetPort(port)
	_, _ = h.mgr.Stop(id)
	_ = os.Remove(h.keyPath(id))
}

// Reach returns the endpoint of a model already running, token included, or
// [ErrNotRunning]. It applies the checks [Host.Serve] applies to a server it finds running,
// so a server below the floor, or one that no longer proves it holds the token, is stopped
// and reported as not running or as an error.
func (h *Host) Reach(ctx context.Context, id string) (Endpoint, error) {
	unlock, err := h.locks.lock(ctx, id)
	if err != nil {
		return Endpoint{}, err
	}
	defer unlock()
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
	defer h.locks.acquire(id)()
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

// readKeys returns the keys a launch wrote for the model, in order: the session token,
// then the probe key. It returns none when the file cannot be read.
func (h *Host) readKeys(id string) []string {
	b, err := os.ReadFile(h.keyPath(id))
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// readToken returns the session token a launch recorded for the model, if the key file is
// still readable and holds one.
func (h *Host) readToken(id string) (string, bool) {
	keys := h.readKeys(id)
	if len(keys) == 0 {
		return "", false
	}
	return keys[0], true
}

// attest shows that the listener at baseURL is the server this host launched, using only
// requests that carry neither the session token nor anything that would let the
// listener's owner use it. The server is started with two keys and accepts either. A
// listener is attributed to the launch when it refuses a request with no credential,
// refuses one with a key nobody issued, and accepts one with the probe key. A squatter
// that answers 200 to everything fails the first, one that accepts any bearer fails the
// second, and one that refuses everything fails the third. Only a process that read the
// key file passes all three, and the file is readable by the owning account alone. The
// probe key reaches the listener only after the first two have shown it enforces a
// credential, and it opens nothing but the server it was written for.
func (h *Host) attest(ctx context.Context, baseURL, probeKey string) error {
	st, err := h.chatStatus(ctx, baseURL, "")
	if err != nil {
		return fmt.Errorf("%w: could not check: %w", ErrUnauthenticated, err)
	}
	if !refused(st) {
		return fmt.Errorf("%w: status %d with no credential", ErrUnauthenticated, st)
	}
	bogus, err := ids.Token()
	if err != nil {
		return err
	}
	st, err = h.chatStatus(ctx, baseURL, bogus)
	if err != nil {
		return fmt.Errorf("%w: could not check: %w", ErrUnauthenticated, err)
	}
	if !refused(st) {
		return fmt.Errorf("%w: status %d with a key nobody issued", ErrUnauthenticated, st)
	}
	st, err = h.chatStatus(ctx, baseURL, probeKey)
	if err != nil {
		return fmt.Errorf("%w: could not check: %w", ErrWrongListener, err)
	}
	if refused(st) {
		return fmt.Errorf("%w: it refused the key written for this launch", ErrWrongListener)
	}
	return nil
}

// refused reports whether a status is an authentication refusal.
func refused(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// chatStatus posts an empty chat request to baseURL with the bearer key, or none when key
// is empty, and returns the status. It never attaches the session token.
func (h *Host) chatStatus(ctx context.Context, baseURL, key string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", strings.NewReader(`{"model":"probe","messages":[]}`))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := h.client(false).Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
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
