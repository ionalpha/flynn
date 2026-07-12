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

// SurfaceProcess is the surface an out-of-process, code-backed extension declares: a
// compiled MCP tool-server binary flynn launches as a confined subprocess and consumes
// over its stdio. It is the concrete resolver for the spec's CodeRef port: where a
// pure-spec surface is data flynn interprets directly, a process surface is a separate
// program flynn runs behind the sandbox and the capability gate. The binary carries no
// authority of its own; every tool it advertises is mounted namespaced, default-deny, and
// governed at the dispatch waist, and the process is treated as potentially compromised
// even when it is first-party.
const SurfaceProcess = "process"

// ProcessBlock is the typed surface block for a process extension. It never carries the
// binary itself or any secret: it names how to obtain the (signed, verified) binary and
// how to run it, and the resolver turns that into a trusted local path. A stored spec is
// therefore safe to inspect and sync.
type ProcessBlock struct {
	// Dev, when set, points at a locally-built binary by absolute path for the extension
	// authoring loop. It is unsigned by nature, so a launcher only honours it when dev mode
	// is explicitly enabled; in a normal run a dev source is refused.
	Dev *DevSource `json:"dev,omitempty"`
	// Release, when set, names a published, signed artifact the resolver downloads and
	// cosign-verifies against a pinned key before it is ever launched (the signed
	// distribution path). Exactly one of Dev or Release is used; Release wins if both are
	// present so a stray dev block cannot downgrade a released extension.
	Release *ReleaseSource `json:"release,omitempty"`
	// Args are fixed arguments appended to the resolved binary path, verbatim. They are
	// part of the spec, never model-influenced, so the launch command line is fully
	// determined before any model runs (closing the stdio-config injection class).
	Args []string `json:"args,omitempty"`
	// Tools, when non-empty, is an allow-list of the advertised tool names to mount; any
	// tool the server exposes that is not listed is ignored. Empty mounts every advertised
	// tool. It is a least-surface control: a spec can pin exactly the tools it vouches for.
	Tools []string `json:"tools,omitempty"`
}

// DevSource is a locally-built extension binary referenced by absolute path.
type DevSource struct {
	Path string `json:"path"`
}

// ReleaseSource names a published extension artifact by its asset name and version. The
// per-os/arch selection, download, and cosign verification are the resolver's job.
type ReleaseSource struct {
	Asset   string `json:"asset"`
	Version string `json:"version"`
}

// Conn is a live duplex connection to a launched extension subprocess: its MCP stdio
// pipes and the means to stop it. The real launcher backs it with a sandbox session over
// anonymous pipes; a test backs it with an in-memory pair. Stop must kill the process and
// leave no orphan.
type Conn interface {
	// Stdin is the process's standard input, for writing MCP requests.
	Stdin() io.WriteCloser
	// Stdout is the process's standard output, for reading MCP replies.
	Stdout() io.Reader
	// Stop ends the process and releases it. It is idempotent.
	Stop() error
}

// diagnoser is the optional half of Conn: a connection that retained why its process died.
// A launcher whose process writes to standard error (a confinement refusal, a bad flag, a
// panic) implements it so a mount failure can say what the process said.
type diagnoser interface {
	// Diagnostics is the retained tail of the process's standard error, or empty.
	Diagnostics() string
}

// maxDiagBytes bounds the process output folded into a mount error. The tail is untrusted
// extension output, so it is bounded like every other value crossing this boundary.
const maxDiagBytes = 4 << 10

// withConnDiagnostics annotates a mount failure with what the extension process printed
// before it died. Without this, a process that exits during the MCP handshake surfaces only
// as an unexplained EOF from the client, and the reason it refused to start (a sandbox that
// could not confine it, an unreadable binary, a bad fixed argument) is lost. The fault's code
// and class are preserved, so callers still switch on them; only the message grows.
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

// LaunchRequest is what the handler hands the launcher to start one extension: the
// verified local binary path, the fixed arguments, and the already-computed effective
// egress allow-list. The handler has already intersected the spec's requested egress with
// the operator grant, so the launcher only enforces the result; it never reads egress from
// the spec directly.
type LaunchRequest struct {
	// Path is the trusted local binary to run. For a released extension it is the
	// cosign-verified artifact; for a dev extension it is the local build (dev mode only).
	Path string
	// Args are the fixed arguments to pass, verbatim.
	Args []string
	// EgressAllow is the effective outbound host allow-list (spec ∩ operator grant). Empty
	// means deny all egress: the extension reaches nothing.
	EgressAllow []string
}

// Launcher starts a verified binary as a confined, egress-locked subprocess and returns a
// duplex connection to its MCP stdio. It is the sandbox boundary of a process extension;
// the concrete SandboxLauncher enforces containment (refuse-rather-than-downgrade),
// deny-by-default egress, and a scrubbed environment, and a test supplies a fake so the
// mount and tool-bridge logic is exercised without a real process.
type Launcher interface {
	Launch(ctx context.Context, req LaunchRequest) (Conn, error)
}

// Resolver turns a process surface into a trusted local binary path plus its arguments.
// It is the trust boundary for the code itself: a released source is downloaded and
// cosign-verified against a pinned key, and a dev source is honoured only when dev mode is
// enabled. A resolver that cannot establish trust returns an error, and the handler then
// never launches anything, so unverified code never runs.
type Resolver interface {
	Resolve(ctx context.Context, extName string, block ProcessBlock) (path string, args []string, err error)
}

// ProcessHandler is the surface handler for out-of-process extensions. It launches an
// extension's verified binary in the sandbox, speaks MCP to it, and mounts its advertised
// tools as governed, namespaced, default-deny flynn tools. It is safe for concurrent use
// and holds one running subprocess per loaded extension id.
type ProcessHandler struct {
	launcher Launcher
	resolver Resolver

	reserved       func(name string) bool
	egressGrant    []string
	callTimeout    time.Duration
	dialTimeout    time.Duration
	maxDescBytes   int
	maxResultBytes int

	// signerFor returns the host key a signing-enabled tool signs with, or nil when the tool
	// does not participate in the host-signing handshake (the default: no tool signs). It is
	// how the operator binds a key to a specific extension tool; default-deny like every other
	// authority here.
	signerFor    func(extName, toolName string) HostSigner
	maxSigns     int
	maxSignBytes int

	// fetcherFor returns the endpoint a network-borrowing tool may reach, or nil when the tool
	// borrows no network (the default: no tool fetches). It is how the operator grants one
	// specific extension tool one specific destination, which the extension itself never names.
	// Default-deny like every other authority here.
	fetcherFor    func(extName, toolName string) HostFetcher
	maxFetches    int
	maxFetchBytes int

	mu      sync.Mutex
	mounted map[string]*mountedProc
}

// mountedProc is the live state for one loaded process extension: the subprocess
// connection, the MCP client speaking to it, and the tools mounted from it.
type mountedProc struct {
	conn   Conn
	client *mcp.Client
	tools  []mission.Tool
}

// ProcessOption configures a ProcessHandler.
type ProcessOption func(*ProcessHandler)

// WithReserved sets the predicate that reports whether a mounted tool name collides with a
// reserved or native name, so an extension cannot shadow a built-in tool (anti
// tool-poisoning). Namespacing already makes a native collision structurally impossible (a
// native tool name carries no dot, a mounted one is always "<ext>.<tool>"); this predicate
// is the belt-and-suspenders check a host wires to its own native-name and reserved-catalog
// set. The default reserves nothing, relying on namespacing alone.
func WithReserved(fn func(name string) bool) ProcessOption {
	return func(h *ProcessHandler) {
		if fn != nil {
			h.reserved = fn
		}
	}
}

// WithEgressGrant sets the operator's outbound host allow-list. The effective egress of any
// extension is its spec's requested hosts intersected with this grant, never the spec's
// hosts alone, so a spec can only narrow what the operator already permits, never widen it.
// Empty (the default) means the operator grants no hosts, so every extension is launched
// with egress fully denied.
func WithEgressGrant(hosts []string) ProcessOption {
	return func(h *ProcessHandler) { h.egressGrant = append([]string(nil), hosts...) }
}

// WithCallTimeout bounds how long a single tool call may take before it is abandoned, so a
// hung extension cannot wedge a run. The default is 60s.
func WithCallTimeout(d time.Duration) ProcessOption {
	return func(h *ProcessHandler) {
		if d > 0 {
			h.callTimeout = d
		}
	}
}

// WithDialTimeout bounds the MCP handshake and tools/list at mount time, so an extension
// that never answers cannot wedge a load. The default is 30s.
func WithDialTimeout(d time.Duration) ProcessOption {
	return func(h *ProcessHandler) {
		if d > 0 {
			h.dialTimeout = d
		}
	}
}

// WithMaxResultBytes bounds the size of a tool result surfaced to the model, so a hostile
// extension cannot flood the model context. The default is 64 KiB.
func WithMaxResultBytes(n int) ProcessOption {
	return func(h *ProcessHandler) {
		if n > 0 {
			h.maxResultBytes = n
		}
	}
}

// WithMaxDescriptionBytes bounds the size of a tool description surfaced to the model, so a
// hostile extension cannot pack instructions into an oversized description. The default is
// 4 KiB.
func WithMaxDescriptionBytes(n int) ProcessOption {
	return func(h *ProcessHandler) {
		if n > 0 {
			h.maxDescBytes = n
		}
	}
}

// WithHostSigner binds host-held signing keys to signing-enabled tools. fn is called for
// each mounted tool and returns the HostSigner that tool may obtain signatures from, or nil
// for a tool that does not sign (the default for every tool). This is how the operator grants
// a specific extension tool the use of a key: the key stays in the host, and the tool only
// ever receives detached signatures over the bytes it hands out. A nil fn leaves every tool
// non-signing.
func WithHostSigner(fn func(extName, toolName string) HostSigner) ProcessOption {
	return func(h *ProcessHandler) { h.signerFor = fn }
}

// WithHostFetcher binds a host-held endpoint to network-borrowing tools. fn is called for each
// mounted tool and returns the HostFetcher that tool may send requests through, or nil for a
// tool that borrows no network (the default for every tool). This is how an extension reaches a
// service without being given egress of its own: the extension process stays network-denied,
// hands out request bytes, and the host sends them to the endpoint IT holds. Because the
// extension never names a destination, a grant is an authority to reach exactly one place. A
// nil fn leaves every tool network-free.
func WithHostFetcher(fn func(extName, toolName string) HostFetcher) ProcessOption {
	return func(h *ProcessHandler) { h.fetcherFor = fn }
}

// WithMaxFetches bounds how many requests one tool call may drive through the host-call
// handshake, so a hostile or broken extension cannot pump the host's network in a loop. The
// default is 256, which is generous for a tool that polls for an on-chain confirmation.
func WithMaxFetches(n int) ProcessOption {
	return func(h *ProcessHandler) {
		if n > 0 {
			h.maxFetches = n
		}
	}
}

// WithMaxSignatures bounds how many signatures one tool call may request through the
// host-signing handshake, so a hostile or broken extension cannot drive an unbounded signing
// loop. The default is 32.
func WithMaxSignatures(n int) ProcessOption {
	return func(h *ProcessHandler) {
		if n > 0 {
			h.maxSigns = n
		}
	}
}

// NewProcessHandler builds a process-surface handler that resolves binaries through
// resolver and launches them through launcher. Both are required; a nil launcher or
// resolver is a programming error the handler refuses to run without.
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

// OnLoad launches one extension's subprocess and mounts its tools. It resolves the binary
// (verified, or dev-only when the resolver allows it), computes the effective egress as the
// spec's requested hosts intersected with the operator grant, launches the process under
// the sandbox, performs the MCP handshake and lists its tools within the dial timeout, and
// wraps each advertised tool as a governed, namespaced, default-deny flynn tool. Any
// failure stops the subprocess before returning, so a failed load leaves no orphan and the
// loader's roll-back sees nothing mounted.
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

	// From here every failure must stop the process, or a rejected load leaks a running
	// subprocess.
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
	// Replacing an already-loaded extension (idempotent reload): stop the old process
	// first so the new one fully supersedes it.
	if prev, ok := h.mounted[m.ID]; ok {
		_ = prev.client.Close()
		_ = prev.conn.Stop()
	}
	h.mounted[m.ID] = &mountedProc{conn: conn, client: client, tools: tools}
	h.mu.Unlock()
	return nil
}

// dialAndBuild performs the MCP handshake, lists the extension's tools, and builds the
// governed flynn tools from them. It enforces the anti-poisoning controls: a tool whose
// namespaced name would shadow a reserved or native name is refused (failing the whole
// load, since a spec that tries to shadow is not one to partly trust), a tool not in the
// spec's allow-list is skipped, and every description is bounded before it can reach a
// model.
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
			continue // not in the spec's allow-list: least-surface
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

// OnUnload stops the extension's subprocess and releases its tools. It is idempotent:
// unloading an id that is not mounted is a no-op, and a Stop error is not surfaced because
// the process is being torn down regardless.
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

// Tools returns the tools mounted for one extension id, satisfying ToolSource so the loader
// surfaces them to the agent. Authority to call any of them is still enforced separately at
// the dispatch waist by the capability grant; being returned here only makes a tool
// reachable, never permitted.
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

// procTool is one extension tool as a governed flynn tool. Its Def name is the namespaced
// "<ext>.<tool>", which is also the action name the dispatch waist admits against the run's
// capability grant, so the tool is default-deny: mounting it makes it reachable, but a run
// calls it only when its grant lists that exact name. Invoke forwards to the subprocess
// over MCP with a per-call deadline and bounds the untrusted result before returning it.
type procTool struct {
	name       string
	remoteName string
	desc       string
	schema     json.RawMessage
	client     *mcp.Client
	timeout    time.Duration
	maxResult  int

	// signer and fetcher are the host authorities this tool was granted; either being non-nil
	// makes Invoke drive the host-call handshake instead of a single call. signer signs the
	// payloads the tool hands out (the key stays here); fetcher sends the request bodies it
	// hands out to the endpoint the fetcher itself holds (the destination is never the tool's).
	// Both are nil by default, which is a tool with neither authority. The max* fields bound how
	// many of each one call may drive, and how large a single payload may be.
	signer        HostSigner
	maxSigns      int
	maxSignBytes  int
	fetcher       HostFetcher
	maxFetches    int
	maxFetchBytes int
}

// Def is the declaration handed to the model. The namespaced name doubles as the capability
// action, so the grant gates this tool by its full name.
func (t *procTool) Def() llm.Tool {
	return llm.Tool{Name: t.name, Description: t.desc, InputSchema: t.schema}
}

// Invoke forwards the call to the extension subprocess over MCP under a per-call deadline
// and returns the tool's result, bounded. A transport failure (timeout, dead peer) is a
// tool error the model sees and adapts to; a tool-level error the extension reports is
// returned as its text with a marker, since the model, not this bridge, decides how to
// react. The result is untrusted extension output and is size-bounded before it is
// returned into any model context or the spine.
// Invoke runs every tool through the host-call loop, whatever it was granted. A tool with no
// grant at all still goes through it, because that is the only place a host-call message can be
// REFUSED: an ungranted tool that asks the host to sign or to send must be stopped, not have its
// request handed back to the model as though it were a result.
func (t *procTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	return t.driveHostCalls(ctx, input)
}

// driveHostCalls runs the host-call handshake for a tool granted a signer, a fetcher, or both.
// It injects the granted key's public bytes on the first call, then services each host-call
// message the tool hands out (signing a payload, or sending a request body to the fetcher's
// own endpoint) and resumes the tool with the result, until the tool returns a result carrying
// no host-call message. That terminal result is bounded and returned to the caller.
//
// The host reads only opaque bytes: it never parses what it signs, and never parses what it
// sends. The key never leaves the host, and the destination is never the extension's to pick.
// Every borrowed authority is bounded: a per-call deadline on each round-trip, a cap on the
// number of signatures and the number of fetches one call may drive, and a cap on the size of
// each payload, so a broken or hostile extension can neither spin the host forever nor make it
// sign or send something over-sized. A tool that asks for an authority it was not granted is
// refused rather than served.
func (t *procTool) driveHostCalls(ctx context.Context, input json.RawMessage) (string, error) {
	next := input
	if t.signer != nil {
		var err error
		if next, err = injectHostKey(input, t.signer.Public()); err != nil {
			return "", err
		}
	}
	signs, fetches := 0, 0
	// The loop bound is the sum of both budgets plus the terminal call, so neither budget can
	// be spent by the other and the loop always terminates.
	for range t.maxSigns + t.maxFetches + 1 {
		res, err := t.call(ctx, next)
		if err != nil {
			return "", err
		}
		if res.IsError {
			return "extension tool error: " + boundText(res.Text, t.maxResult), nil
		}
		var reply hostCallReply
		// A result that is not a host-call message (not valid JSON, or valid JSON with neither a
		// sign nor a fetch block) is the terminal result: return it to the caller untouched but
		// bounded. A parse failure is not an error here; it just means this result is terminal.
		_ = json.Unmarshal([]byte(res.Text), &reply)
		switch {
		case reply.Sign != nil && reply.Fetch != nil:
			return "", fault.New(fault.Terminal, "extension_hostcall_ambiguous",
				"extension: tool "+t.name+" asked to sign and fetch in one message")
		case reply.Sign != nil:
			signs++
			if next, err = t.serviceSign(reply, signs); err != nil {
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

// serviceSign signs one payload the tool handed out and returns the resume input. A signing
// failure is delivered to the tool (not aborted here) so the tool's own failure path runs; the
// tool decides how to unwind.
func (t *procTool) serviceSign(reply hostCallReply, n int) (json.RawMessage, error) {
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
	sig, signErr := t.signer.Sign(payload)
	return resumeSign(reply.Session, sig, signErr)
}

// serviceFetch sends one request body the tool handed out to the granted fetcher's own
// endpoint and returns the resume input. Like a signing failure, a fetch failure is delivered
// to the tool rather than aborting here, so a tool that has already acted (created a mint, say)
// still runs its unwind path instead of being cut off mid-flight.
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

// call forwards one message to the subprocess under a fresh per-call deadline. Each round-trip
// of the signing handshake gets its own deadline so a tool that legitimately blocks on one step
// (waiting for an on-chain confirmation, say) is not starved by earlier steps' time.
func (t *procTool) call(ctx context.Context, input json.RawMessage) (mcp.CallResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	res, err := t.client.CallTool(callCtx, t.remoteName, input)
	if err != nil {
		return mcp.CallResult{}, fault.Wrap(fault.Transient, "extension_process_call", err)
	}
	return res, nil
}

// intersectHosts returns the hosts allowed by BOTH the spec's request and the operator
// grant, the no-escalation rule for egress: an extension can only narrow the operator's
// allow-list, never widen it. A nil or empty grant yields no hosts (deny all), since the
// operator has permitted nothing. Comparison is case-insensitive on the trimmed name.
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

// nameSet builds a lookup set from a list of names, or nil for an empty list.
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

// boundText size-bounds untrusted text and strips ASCII control characters other than tab
// and newline, so a hostile extension can neither flood the model context nor smuggle
// terminal-control or other non-printing sequences into it through a description or result.
// The content is still treated as data, never instructions; this is the size-and-shape
// bound on top of that structural guarantee.
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
			continue // drop other control characters
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > limit {
		return out[:limit] + fmt.Sprintf("\n...[truncated %d bytes]", len(out)-limit)
	}
	return out
}
