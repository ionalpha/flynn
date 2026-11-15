package extension

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mcp"
	"github.com/ionalpha/flynn/mission"
)

// SurfaceProcess declares a compiled MCP tool server that flynn runs as a
// sandboxed subprocess over stdio. The binary has no authority of its own: its
// tools mount namespaced and default-deny behind the dispatch waist, and the
// process is treated as possibly compromised even when first-party.
const SurfaceProcess = "process"

// ProcessBlock is the surface block for a process extension. It names where to
// get the verified binary and how to run it, never the binary or a secret, so
// a stored spec is safe to sync.
type ProcessBlock struct {
	// Dev is an unsigned local build for authoring, honoured only in dev mode.
	Dev *DevSource `json:"dev,omitempty"`
	// Release is a signed artifact, cosign-verified against a pinned key before
	// launch. It wins over Dev so a stray dev block cannot downgrade it.
	Release *ReleaseSource `json:"release,omitempty"`
	// Args are passed verbatim. They come from the spec alone, so no model
	// output can inject into the command line.
	Args []string `json:"args,omitempty"`
	// Tools, when non-empty, allow-lists the advertised tools to mount; empty
	// mounts all.
	Tools []string `json:"tools,omitempty"`
}

// DevSource is a locally-built extension binary referenced by absolute path.
type DevSource struct {
	Path string `json:"path"`
}

// ReleaseSource names a published artifact; the resolver picks the platform,
// downloads and verifies it.
type ReleaseSource struct {
	Asset   string `json:"asset"`
	Version string `json:"version"`

	// Digests maps "<goos>_<goarch>" to the archive's SHA-256. A tag is
	// mutable, and the signature proves only which workflow built an archive,
	// so a re-cut tag would verify; the digest refuses it.
	//
	// A platform missing from the map falls back to signature-only trust.
	Digests map[string]string `json:"digests,omitempty"`
}

// Conn is the MCP stdio of a launched extension subprocess. Stop must kill the
// process and leave no orphan.
type Conn interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// Stop is idempotent.
	Stop() error
}

// diagnoser is an optional [Conn] method set that keeps the process's stderr
// so a mount failure can report it.
type diagnoser interface {
	// Diagnostics is the retained stderr tail, or empty.
	Diagnostics() string
}

// maxDiagBytes bounds the untrusted stderr tail folded into a mount error.
const maxDiagBytes = 4 << 10

// withConnDiagnostics appends the process's stderr to a mount failure, which
// would otherwise be a bare EOF from the handshake. The fault code and class
// are kept.
func withConnDiagnostics(err error, conn Conn) error {
	d, ok := conn.(diagnoser)
	if !ok {
		return err
	}
	tail := strings.TrimSpace(d.Diagnostics())
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w (extension process said: %s)", err, boundText(tail, maxDiagBytes))
}

// LaunchRequest starts one extension. The handler has already intersected the
// spec's egress with the operator grant; the launcher only enforces it.
type LaunchRequest struct {
	// Path is the verified artifact, or the local build in dev mode.
	Path string
	Args []string
	// EgressAllow is spec ∩ operator grant; empty denies all egress.
	EgressAllow []string
}

// Launcher starts a verified binary confined and egress-locked. [SandboxLauncher]
// refuses rather than downgrading containment, denies egress by default and
// scrubs the environment.
type Launcher interface {
	Launch(ctx context.Context, req LaunchRequest) (Conn, error)
}

// Resolver turns a [ProcessBlock] into a trusted local path and arguments: a
// release is cosign-verified against a pinned key, a dev source honoured only
// in dev mode. On error the handler launches nothing.
type Resolver interface {
	Resolve(ctx context.Context, extName string, block ProcessBlock) (path string, args []string, err error)
}

// ProcessHandler runs process extensions in the sandbox and mounts their MCP
// tools namespaced and default-deny. It is safe for concurrent use and keeps
// one subprocess per loaded extension id.
type ProcessHandler struct {
	launcher Launcher
	resolver Resolver

	reserved       func(name string) bool
	egressGrant    []string
	callTimeout    time.Duration
	dialTimeout    time.Duration
	maxDescBytes   int
	maxResultBytes int

	// signerFor and fetcherFor return nil for a tool the operator granted
	// nothing, which is every tool by default.
	signerFor    func(extName, toolName string) HostSigner
	maxSigns     int
	maxSignBytes int

	fetcherFor    func(extName, toolName string) HostFetcher
	maxFetches    int
	maxFetchBytes int

	mu      sync.Mutex
	mounted map[string]*mountedProc
}

type mountedProc struct {
	conn   Conn
	client *mcp.Client
	tools  []mission.Tool
}

// ProcessOption configures a ProcessHandler.
type ProcessOption func(*ProcessHandler)

// WithReserved sets a predicate for reserved tool names an extension may not
// shadow. Mounted names are "<ext>.<tool>" and native ones have no dot, so
// this is a second check; the default reserves nothing.
func WithReserved(fn func(name string) bool) ProcessOption {
	return func(h *ProcessHandler) {
		if fn != nil {
			h.reserved = fn
		}
	}
}

// WithEgressGrant sets the operator's host allow-list, which each spec's hosts
// are intersected with, so a spec can only narrow it. The default empty grant
// denies all egress.
func WithEgressGrant(hosts []string) ProcessOption {
	return func(h *ProcessHandler) { h.egressGrant = append([]string(nil), hosts...) }
}

// WithCallTimeout bounds one tool call so a hung extension cannot stall a
// run. The default is 60s.
func WithCallTimeout(d time.Duration) ProcessOption {
	return func(h *ProcessHandler) {
		if d > 0 {
			h.callTimeout = d
		}
	}
}

// WithDialTimeout bounds the mount-time handshake and tools/list. The default
// is 30s.
func WithDialTimeout(d time.Duration) ProcessOption {
	return func(h *ProcessHandler) {
		if d > 0 {
			h.dialTimeout = d
		}
	}
}

// WithMaxResultBytes bounds a tool result shown to the model. The default is
// 64 KiB.
func WithMaxResultBytes(n int) ProcessOption {
	return func(h *ProcessHandler) {
		if n > 0 {
			h.maxResultBytes = n
		}
	}
}

// WithMaxDescriptionBytes bounds a tool description, limiting room for
// injected instructions. The default is 4 KiB.
func WithMaxDescriptionBytes(n int) ProcessOption {
	return func(h *ProcessHandler) {
		if n > 0 {
			h.maxDescBytes = n
		}
	}
}

// WithHostSigner sets fn, which returns the [HostSigner] a mounted tool may
// use, or nil. The key stays in the host; the tool only gets detached
// signatures over bytes it supplies. A nil fn grants no tool a signer.
func WithHostSigner(fn func(extName, toolName string) HostSigner) ProcessOption {
	return func(h *ProcessHandler) { h.signerFor = fn }
}

// WithHostFetcher sets fn, which returns the [HostFetcher] a mounted tool may
// send requests through, or nil. The process stays network-denied and the host
// picks the destination, so a grant reaches exactly one place. A nil fn grants
// no tool a fetcher.
func WithHostFetcher(fn func(extName, toolName string) HostFetcher) ProcessOption {
	return func(h *ProcessHandler) { h.fetcherFor = fn }
}

// WithMaxFetches bounds host fetches per tool call. The default, 256, allows a
// tool polling for an on-chain confirmation.
func WithMaxFetches(n int) ProcessOption {
	return func(h *ProcessHandler) {
		if n > 0 {
			h.maxFetches = n
		}
	}
}

// WithMaxSignatures bounds host signatures per tool call. The default is 32.
func WithMaxSignatures(n int) ProcessOption {
	return func(h *ProcessHandler) {
		if n > 0 {
			h.maxSigns = n
		}
	}
}

// NewProcessHandler returns a handler using resolver and launcher. Both are
// required; OnLoad refuses to run with either nil.
func NewProcessHandler(launcher Launcher, resolver Resolver, opts ...ProcessOption) *ProcessHandler {
	h := &ProcessHandler{
		launcher:       launcher,
		resolver:       resolver,
		reserved:       func(string) bool { return false },
		callTimeout:    60 * time.Second,
		dialTimeout:    30 * time.Second,
		maxDescBytes:   4 << 10,
		maxResultBytes: 64 << 10,
		maxSigns:       32,
		maxSignBytes:   64 << 10,
		maxFetches:     256,
		maxFetchBytes:  256 << 10,
		mounted:        map[string]*mountedProc{},
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Capability returns the surface key this handler serves.
func (h *ProcessHandler) Capability() string { return SurfaceProcess }

// OnLoad resolves, launches and mounts one extension, with egress set to the
// spec's hosts intersected with the operator grant. Any failure stops the
// subprocess first, so a failed load leaves no orphan and nothing mounted.
func (h *ProcessHandler) OnLoad(ctx context.Context, m Mount) error {
	if h.launcher == nil || h.resolver == nil {
		return fault.New(fault.Terminal, "extension_process_unconfigured",
			"extension: process handler has no launcher or resolver")
	}
	var block ProcessBlock
	if err := json.Unmarshal(m.Block, &block); err != nil {
		return fault.Wrap(fault.Terminal, "extension_process_block", err)
	}

	path, args, err := h.resolver.Resolve(ctx, m.Name, block)
	if err != nil {
		return fault.Wrap(fault.Terminal, "extension_process_resolve", err)
	}

	egress := intersectHosts(m.Spec.Safety.EgressAllow, h.egressGrant)

	conn, err := h.launcher.Launch(ctx, LaunchRequest{Path: path, Args: args, EgressAllow: egress})
	if err != nil {
		return fault.Wrap(fault.Terminal, "extension_process_launch", err)
	}

	// From here every failure must stop the process.
	client := mcp.NewClient(conn.Stdout(), conn.Stdin(), mcp.WithClientInfo(mcp.Info{Name: "flynn", Version: ""}))
	dialCtx, cancel := context.WithTimeout(ctx, h.dialTimeout)
	defer cancel()

	tools, err := h.dialAndBuild(dialCtx, m, block, client)
	if err != nil {
		err = withConnDiagnostics(err, conn)
		_ = client.Close()
		_ = conn.Stop()
		return err
	}

	h.mu.Lock()
	// A reload stops the old process so the new one replaces it.
	if prev, ok := h.mounted[m.ID]; ok {
		_ = prev.client.Close()
		_ = prev.conn.Stop()
	}
	h.mounted[m.ID] = &mountedProc{conn: conn, client: client, tools: tools}
	h.mu.Unlock()
	return nil
}

// dialAndBuild handshakes, lists and wraps the extension's tools. A tool
// shadowing a reserved name fails the whole load, since a spec that tries it
// is not partly trusted; unlisted tools are skipped and descriptions bounded.
func (h *ProcessHandler) dialAndBuild(ctx context.Context, m Mount, block ProcessBlock, client *mcp.Client) ([]mission.Tool, error) {
	if _, err := client.Initialize(ctx); err != nil {
		return nil, fault.Wrap(fault.Transient, "extension_process_handshake", err)
	}
	descs, err := client.ListTools(ctx)
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "extension_process_list", err)
	}

	allow := nameSet(block.Tools)
	out := make([]mission.Tool, 0, len(descs))
	for _, d := range descs {
		if d.Name == "" {
			continue
		}
		if len(allow) > 0 && !allow[d.Name] {
			continue
		}
		// A signer's tools are only the host's to call, over SignerChannel. Mounted,
		// the model could unlock the key or get signatures directly, bypassing the
		// worker and its capability grant.
		if reservedSignerTool(d.Name) {
			continue
		}
		full := m.Name + "." + d.Name
		if h.reserved != nil && (h.reserved(full) || h.reserved(d.Name)) {
			return nil, fault.New(fault.Forbidden, "extension_process_reserved",
				"extension: tool "+full+" collides with a reserved name")
		}
		schema := d.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		var signer HostSigner
		if h.signerFor != nil {
			signer = h.signerFor(m.Name, d.Name)
		}
		var fetcher HostFetcher
		if h.fetcherFor != nil {
			fetcher = h.fetcherFor(m.Name, d.Name)
		}
		out = append(out, &procTool{
			name:          full,
			remoteName:    d.Name,
			desc:          boundText(d.Description, h.maxDescBytes),
			schema:        schema,
			client:        client,
			timeout:       h.callTimeout,
			maxResult:     h.maxResultBytes,
			signer:        signer,
			maxSigns:      h.maxSigns,
			maxSignBytes:  h.maxSignBytes,
			fetcher:       fetcher,
			maxFetches:    h.maxFetches,
			maxFetchBytes: h.maxFetchBytes,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Def().Name < out[j].Def().Name })
	return out, nil
}

// OnUnload stops the subprocess and drops its tools. An unknown id is a no-op,
// and a Stop error is ignored since teardown proceeds regardless.
func (h *ProcessHandler) OnUnload(_ context.Context, id string) error {
	h.mu.Lock()
	p, ok := h.mounted[id]
	delete(h.mounted, id)
	h.mu.Unlock()
	if !ok {
		return nil
	}
	_ = p.client.Close()
	_ = p.conn.Stop()
	return nil
}

// Tools returns the tools mounted for id. Returning a tool makes it reachable;
// the capability grant at the dispatch waist still decides whether a run may
// call it.
func (h *ProcessHandler) Tools(id string) []mission.Tool {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.mounted[id]
	if !ok {
		return nil
	}
	out := make([]mission.Tool, len(p.tools))
	copy(out, p.tools)
	return out
}

// procTool is one extension tool. Its name "<ext>.<tool>" is also the
// capability action, so a run calls it only when its grant lists that name.
type procTool struct {
	name       string
	remoteName string
	desc       string
	schema     json.RawMessage
	client     *mcp.Client
	timeout    time.Duration
	maxResult  int

	// signer and fetcher are the tool's granted host authorities, nil by
	// default. The max fields cap the count per call and each payload's size.
	signer        HostSigner
	maxSigns      int
	maxSignBytes  int
	fetcher       HostFetcher
	maxFetches    int
	maxFetchBytes int
}

// Def is the declaration handed to the model.
func (t *procTool) Def() llm.Tool {
	return llm.Tool{Name: t.name, Description: t.desc, InputSchema: t.schema}
}

// Invoke calls the tool over MCP with a per-call deadline and returns its
// result bounded. A tool-reported error is returned as marked text for the
// model to handle. Every tool goes through the host-call loop, granted or
// not, because only there can an ungranted sign or fetch request be refused
// instead of reaching the model as a result.
func (t *procTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	return t.driveHostCalls(ctx, input)
}

// driveHostCalls injects the signer's public key on the first call, then
// services each sign or fetch message and resumes the tool, until a result
// carries no host-call message. That result is bounded and returned.
//
// The host treats payloads as opaque bytes, keeps the key, and picks the
// destination. Each round-trip has a deadline, and the counts and payload
// sizes are capped, so a hostile extension cannot spin the host or make it
// sign or send oversized data. An ungranted request is refused.
func (t *procTool) driveHostCalls(ctx context.Context, input json.RawMessage) (string, error) {
	next := input
	if t.signer != nil {
		var err error
		if next, err = injectHostKey(input, t.signer.Public()); err != nil {
			return "", err
		}
	}
	signs, fetches := 0, 0
	// Both budgets plus the terminal call, so the loop always ends and neither
	// budget is spent by the other.
	for range t.maxSigns + t.maxFetches + 1 {
		res, err := t.call(ctx, next)
		if err != nil {
			return "", err
		}
		if res.IsError {
			return "extension tool error: " + boundText(res.Text, t.maxResult), nil
		}
		// A result that does not parse as a sign or fetch message is terminal.
		reply := parseHostCall(res.Text)
		switch {
		case reply.Sign != nil && reply.Fetch != nil:
			return "", fault.New(fault.Terminal, "extension_hostcall_ambiguous",
				"extension: tool "+t.name+" asked to sign and fetch in one message")
		case reply.Sign != nil:
			signs++
			if next, err = t.serviceSign(ctx, reply, signs); err != nil {
				return "", err
			}
		case reply.Fetch != nil:
			fetches++
			if next, err = t.serviceFetch(ctx, reply, fetches); err != nil {
				return "", err
			}
		default:
			return boundText(res.Text, t.maxResult), nil
		}
	}
	return "", fault.New(fault.Terminal, "extension_hostcall_loop",
		"extension: host-call loop ended without a result")
}

// serviceSign signs one payload and returns the resume input. A signing
// failure goes back to the tool so its own failure path unwinds.
func (t *procTool) serviceSign(ctx context.Context, reply hostCallReply, n int) (json.RawMessage, error) {
	if t.signer == nil {
		return nil, fault.New(fault.Forbidden, "extension_sign_ungranted",
			"extension: tool "+t.name+" asked to sign but was granted no key")
	}
	if n > t.maxSigns {
		return nil, fault.New(fault.Forbidden, "extension_sign_budget",
			"extension: tool "+t.name+" exceeded the per-call signature limit")
	}
	payload, err := base64.StdEncoding.DecodeString(reply.Sign.Message)
	if err != nil {
		return nil, fault.New(fault.Terminal, "extension_sign_payload", "extension: signing payload is not base64")
	}
	if len(payload) > t.maxSignBytes {
		return nil, fault.New(fault.Forbidden, "extension_sign_too_large",
			"extension: tool "+t.name+" asked to sign an over-sized payload")
	}
	// Something must judge the payload. The producing tool may be compromised,
	// and the host parses no payload format so it stays general, which leaves
	// the signer. A signer that does not judge would be blind signing, so it
	// is refused.
	if !selfPoliced(t.signer) {
		return nil, fault.New(fault.Forbidden, "extension_sign_unpoliced",
			"extension: tool "+t.name+" is wired to a signer that does not judge what it signs, and the host holds no parser to judge it for them, so nobody would be reading the payload at all")
	}
	sig, signErr := t.signer.Sign(ctx, payload)
	return resumeSign(reply.Session, sig, signErr)
}

// selfPoliced reports whether s implements [SelfPolicing] and judges its own
// payloads.
func selfPoliced(s HostSigner) bool {
	sp, ok := s.(SelfPolicing)
	return ok && sp.PolicesPayloads()
}

// serviceFetch sends one body to the fetcher's endpoint and returns the resume
// input. A fetch failure goes back to the tool, so a tool that has already
// acted can still unwind.
func (t *procTool) serviceFetch(ctx context.Context, reply hostCallReply, n int) (json.RawMessage, error) {
	if t.fetcher == nil {
		return nil, fault.New(fault.Forbidden, "extension_fetch_ungranted",
			"extension: tool "+t.name+" asked to fetch but was granted no endpoint")
	}
	if n > t.maxFetches {
		return nil, fault.New(fault.Forbidden, "extension_fetch_budget",
			"extension: tool "+t.name+" exceeded the per-call fetch limit")
	}
	body, err := base64.StdEncoding.DecodeString(reply.Fetch.Body)
	if err != nil {
		return nil, fault.New(fault.Terminal, "extension_fetch_payload", "extension: fetch body is not base64")
	}
	if len(body) > t.maxFetchBytes {
		return nil, fault.New(fault.Forbidden, "extension_fetch_too_large",
			"extension: tool "+t.name+" asked to send an over-sized request")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	res, fetchErr := t.fetcher.Fetch(fetchCtx, body)
	return resumeFetch(reply.Session, res, fetchErr)
}

// call gives each round-trip its own deadline, so a step that blocks on an
// on-chain confirmation is not starved by earlier steps.
func (t *procTool) call(ctx context.Context, input json.RawMessage) (mcp.CallResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	res, err := t.client.CallTool(callCtx, t.remoteName, input)
	if err != nil {
		return mcp.CallResult{}, fault.Wrap(fault.Transient, "extension_process_call", err)
	}
	return res, nil
}

// intersectHosts returns the requested hosts the grant also allows, compared
// case-insensitively after trimming, so an extension can never widen egress.
// An empty grant yields none.
func intersectHosts(requested, grant []string) []string {
	if len(requested) == 0 || len(grant) == 0 {
		return nil
	}
	granted := make(map[string]struct{}, len(grant))
	for _, g := range grant {
		granted[strings.ToLower(strings.TrimSpace(g))] = struct{}{}
	}
	seen := make(map[string]struct{}, len(requested))
	out := make([]string, 0, len(requested))
	for _, r := range requested {
		key := strings.ToLower(strings.TrimSpace(r))
		if key == "" {
			continue
		}
		if _, ok := granted[key]; !ok {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func nameSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			set[n] = true
		}
	}
	return set
}

// boundText caps untrusted text at limit bytes and strips ASCII controls but
// tab and newline, so an extension cannot flood the context or smuggle
// terminal sequences.
func boundText(s string, limit int) string {
	if limit <= 0 {
		limit = 64 << 10
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > limit {
		return out[:limit] + fmt.Sprintf("\n...[truncated %d bytes]", len(out)-limit)
	}
	return out
}
