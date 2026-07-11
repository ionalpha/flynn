package extension

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mcp"
	"github.com/ionalpha/flynn/mission"
)

// stubTool is one tool a stub extension server advertises, with a controllable behaviour so
// a test can make the extension hostile: a huge result, a hang, a nasty description, or a
// reported error.
type stubTool struct {
	name   string
	desc   string
	invoke func(ctx context.Context, input json.RawMessage) (string, error)
}

func (t stubTool) Def() llm.Tool {
	return llm.Tool{Name: t.name, Description: t.desc, InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t stubTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	return t.invoke(ctx, input)
}

// fakeConn wires the handler's MCP client to an in-process mcp.Server running the stub
// tools, over two pipes: it stands in for the sandbox session without launching a process.
// Stop cancels the server and closes the pipes, and records that it was stopped so a test
// can prove no orphan is left after a failed or unloaded mount.
type fakeConn struct {
	stdin   io.WriteCloser
	stdout  io.Reader
	cancel  context.CancelFunc
	closers []io.Closer

	mu      sync.Mutex
	stopped bool
}

func (c *fakeConn) Stdin() io.WriteCloser { return c.stdin }
func (c *fakeConn) Stdout() io.Reader     { return c.stdout }

func (c *fakeConn) Stop() error {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.cancel()
	for _, cl := range c.closers {
		_ = cl.Close()
	}
	return nil
}

func (c *fakeConn) wasStopped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// newFakeConn builds a fakeConn serving the given stub tools through a real mcp.Server.
func newFakeConn(tools []mission.Tool) *fakeConn {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	srv := mcp.NewServer(nil, tools, mcp.WithInfo(mcp.Info{Name: "stub-ext", Version: "0.1"}))
	go func() { _ = srv.Serve(ctx, serverR, serverW) }()
	return &fakeConn{
		stdin:   clientW,
		stdout:  clientR,
		cancel:  cancel,
		closers: []io.Closer{clientW, serverW},
	}
}

// fakeLauncher hands back a preconfigured fakeConn and records the LaunchRequest it was
// given, so a test can assert exactly what egress and command reached the sandbox boundary.
// A launchErr makes the launch itself fail.
type fakeLauncher struct {
	conn      *fakeConn
	launchErr error

	mu   sync.Mutex
	last LaunchRequest
	n    int
}

func (l *fakeLauncher) Launch(_ context.Context, req LaunchRequest) (Conn, error) {
	l.mu.Lock()
	l.last = req
	l.n++
	l.mu.Unlock()
	if l.launchErr != nil {
		return nil, l.launchErr
	}
	return l.conn, nil
}

func (l *fakeLauncher) lastRequest() LaunchRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last
}

// okResolver returns a fixed trusted path, standing in for a verified binary so the test
// exercises the mount and tool-bridge, not the resolver.
type okResolver struct{ args []string }

func (r okResolver) Resolve(_ context.Context, _ string, _ ProcessBlock) (string, []string, error) {
	return "/verified/bin", r.args, nil
}

func mountStub(t *testing.T, tools []mission.Tool, opts ...ProcessOption) (*ProcessHandler, *fakeConn, Mount) {
	t.Helper()
	conn := newFakeConn(tools)
	h := NewProcessHandler(&fakeLauncher{conn: conn}, okResolver{}, opts...)
	m := Mount{ID: "ext-1", Name: "token", Spec: Spec{}, Surface: SurfaceProcess, Block: json.RawMessage(`{}`)}
	if err := h.OnLoad(context.Background(), m); err != nil {
		t.Fatalf("OnLoad: %v", err)
	}
	t.Cleanup(func() { _ = h.OnUnload(context.Background(), m.ID) })
	return h, conn, m
}

func echoStub() []mission.Tool {
	return []mission.Tool{stubTool{
		name: "mint", desc: "mint a token",
		invoke: func(_ context.Context, in json.RawMessage) (string, error) { return "minted:" + string(in), nil },
	}}
}

// TestProcessMountNamespacesTools proves an extension's tools are mounted under the
// extension's namespace, so the model-facing name is "<ext>.<tool>" and cannot be the bare
// name of a native tool.
func TestProcessMountNamespacesTools(t *testing.T) {
	h, _, m := mountStub(t, echoStub())
	tools := h.Tools(m.ID)
	if len(tools) != 1 {
		t.Fatalf("mounted %d tools, want 1", len(tools))
	}
	if got := tools[0].Def().Name; got != "token.mint" {
		t.Fatalf("tool name = %q, want token.mint", got)
	}
}

// TestProcessToolIsCapabilityGated proves the mounted tool is default-deny at the dispatch
// waist: its action name is the namespaced name, which a run's grant must list explicitly.
// A grant that omits it is denied; a grant that includes it is admitted. This is control C2.
func TestProcessToolIsCapabilityGated(t *testing.T) {
	h, _, m := mountStub(t, echoStub())
	name := h.Tools(m.ID)[0].Def().Name // token.mint

	adm := capability.Admitter{}
	action := dispatch.Action{Name: name}

	denyCtx := capability.Into(context.Background(), capability.NewGrant("some.other.action"))
	if err := adm.Admit(denyCtx, action); err == nil {
		t.Fatalf("expected %q to be denied by a grant that omits it", name)
	}
	allowCtx := capability.Into(context.Background(), capability.NewGrant(name))
	if err := adm.Admit(allowCtx, action); err != nil {
		t.Fatalf("expected %q to be admitted by a grant that lists it: %v", name, err)
	}
}

// TestProcessReservedNameRefused proves an extension cannot mount a tool whose namespaced
// name collides with a reserved or native name: the whole load fails and the subprocess is
// stopped, leaving no orphan. This is the anti-shadow control C6.
func TestProcessReservedNameRefused(t *testing.T) {
	conn := newFakeConn([]mission.Tool{stubTool{
		name: "shadow", desc: "d",
		invoke: func(context.Context, json.RawMessage) (string, error) { return "", nil },
	}})
	h := NewProcessHandler(&fakeLauncher{conn: conn}, okResolver{},
		WithReserved(func(n string) bool { return n == "token.shadow" }))
	m := Mount{ID: "ext-1", Name: "token", Surface: SurfaceProcess, Block: json.RawMessage(`{}`)}
	if err := h.OnLoad(context.Background(), m); err == nil {
		t.Fatal("expected the load to fail on a reserved-name collision")
	}
	if !conn.wasStopped() {
		t.Fatal("subprocess was not stopped after a failed load: orphan leak")
	}
}

// TestProcessResultBounded proves a hostile tool result is size-bounded before it reaches
// the model, so an extension cannot flood the model context. This is control C6/C7.
func TestProcessResultBounded(t *testing.T) {
	huge := strings.Repeat("A", 1<<20)
	tools := []mission.Tool{stubTool{
		name: "flood", desc: "d",
		invoke: func(context.Context, json.RawMessage) (string, error) { return huge, nil },
	}}
	h, _, m := mountStub(t, tools, WithMaxResultBytes(1024))
	out, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if len(out) > 1024+64 { // the truncation marker adds a little
		t.Fatalf("result not bounded: %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Fatal("expected a truncation marker on the bounded result")
	}
}

// TestProcessDescriptionSanitised proves a hostile tool description is bounded and stripped
// of control characters before it is surfaced to the model, so an extension cannot pack an
// oversized or terminal-control-laden description into model context. This is the
// anti-tool-poisoning control C6.
func TestProcessDescriptionSanitised(t *testing.T) {
	nasty := "do X\x1b[31m\x00\x07" + strings.Repeat("B", 10000)
	tools := []mission.Tool{stubTool{
		name: "t", desc: nasty,
		invoke: func(context.Context, json.RawMessage) (string, error) { return "", nil },
	}}
	h, _, m := mountStub(t, tools, WithMaxDescriptionBytes(256))
	got := h.Tools(m.ID)[0].Def().Description
	if len(got) > 256+64 {
		t.Fatalf("description not bounded: %d bytes", len(got))
	}
	if strings.ContainsAny(got, "\x00\x07\x1b") {
		t.Fatalf("description retained control characters: %q", got)
	}
}

// TestProcessCallTimeoutDoesNotHang proves a tool call to a hung extension is abandoned at
// the per-call deadline rather than wedging the run. This is control C7.
func TestProcessCallTimeoutDoesNotHang(t *testing.T) {
	tools := []mission.Tool{stubTool{
		name: "hang", desc: "d",
		invoke: func(ctx context.Context, _ json.RawMessage) (string, error) {
			<-ctx.Done() // never return until the server context is cancelled
			return "", ctx.Err()
		},
	}}
	h, _, m := mountStub(t, tools, WithCallTimeout(150*time.Millisecond))

	done := make(chan error, 1)
	go func() {
		_, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error from a hung tool")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Invoke did not return after its per-call deadline: it hung")
	}
}

// TestProcessToolAllowList proves the spec's tool allow-list is honoured: a tool the server
// advertises that is not listed is not mounted (least surface).
func TestProcessToolAllowList(t *testing.T) {
	tools := []mission.Tool{
		stubTool{name: "keep", desc: "d", invoke: func(context.Context, json.RawMessage) (string, error) { return "", nil }},
		stubTool{name: "drop", desc: "d", invoke: func(context.Context, json.RawMessage) (string, error) { return "", nil }},
	}
	conn := newFakeConn(tools)
	h := NewProcessHandler(&fakeLauncher{conn: conn}, okResolver{})
	m := Mount{ID: "ext-1", Name: "token", Surface: SurfaceProcess, Block: json.RawMessage(`{"tools":["keep"]}`)}
	if err := h.OnLoad(context.Background(), m); err != nil {
		t.Fatalf("OnLoad: %v", err)
	}
	t.Cleanup(func() { _ = h.OnUnload(context.Background(), m.ID) })
	mounted := h.Tools(m.ID)
	if len(mounted) != 1 || mounted[0].Def().Name != "token.keep" {
		t.Fatalf("allow-list not honoured: %+v", mounted)
	}
}

// TestProcessUnloadStopsProcess proves unloading an extension stops its subprocess, so a
// disabled extension leaves no orphan.
func TestProcessUnloadStopsProcess(t *testing.T) {
	conn := newFakeConn(echoStub())
	h := NewProcessHandler(&fakeLauncher{conn: conn}, okResolver{})
	m := Mount{ID: "ext-1", Name: "token", Surface: SurfaceProcess, Block: json.RawMessage(`{}`)}
	if err := h.OnLoad(context.Background(), m); err != nil {
		t.Fatalf("OnLoad: %v", err)
	}
	if err := h.OnUnload(context.Background(), m.ID); err != nil {
		t.Fatalf("OnUnload: %v", err)
	}
	if !conn.wasStopped() {
		t.Fatal("subprocess was not stopped on unload")
	}
	// A second unload is a no-op.
	if err := h.OnUnload(context.Background(), m.ID); err != nil {
		t.Fatalf("second unload: %v", err)
	}
}

// TestProcessEgressIntersected proves the effective egress handed to the sandbox is the
// spec's requested hosts intersected with the operator grant, never the spec's alone: a
// host the spec wants but the operator has not granted is dropped. This is control C3.
func TestProcessEgressIntersected(t *testing.T) {
	conn := newFakeConn(echoStub())
	fl := &fakeLauncher{conn: conn}
	h := NewProcessHandler(fl, okResolver{}, WithEgressGrant([]string{"api.mainnet-beta.solana.com"}))
	m := Mount{
		ID: "ext-1", Name: "token", Surface: SurfaceProcess, Block: json.RawMessage(`{}`),
		Spec: Spec{Safety: SafetySpec{EgressAllow: []string{"api.mainnet-beta.solana.com", "evil.example.com"}}},
	}
	if err := h.OnLoad(context.Background(), m); err != nil {
		t.Fatalf("OnLoad: %v", err)
	}
	t.Cleanup(func() { _ = h.OnUnload(context.Background(), m.ID) })
	got := fl.lastRequest().EgressAllow
	if len(got) != 1 || got[0] != "api.mainnet-beta.solana.com" {
		t.Fatalf("effective egress = %v, want only the granted host", got)
	}
}

// TestProcessEgressDeniedWithoutGrant proves that with no operator egress grant, an
// extension is launched with egress fully denied regardless of what its spec requests: a
// spec cannot self-authorise reach. This is the deny-by-default half of C3.
func TestProcessEgressDeniedWithoutGrant(t *testing.T) {
	conn := newFakeConn(echoStub())
	fl := &fakeLauncher{conn: conn}
	h := NewProcessHandler(fl, okResolver{}) // no egress grant
	m := Mount{
		ID: "ext-1", Name: "token", Surface: SurfaceProcess, Block: json.RawMessage(`{}`),
		Spec: Spec{Safety: SafetySpec{EgressAllow: []string{"anything.example.com"}}},
	}
	if err := h.OnLoad(context.Background(), m); err != nil {
		t.Fatalf("OnLoad: %v", err)
	}
	t.Cleanup(func() { _ = h.OnUnload(context.Background(), m.ID) })
	if got := fl.lastRequest().EgressAllow; len(got) != 0 {
		t.Fatalf("effective egress = %v, want none (deny-all) without an operator grant", got)
	}
}

// TestProcessLaunchesVerifiedPath proves the handler launches exactly what the resolver
// returns (a verified path plus fixed args) and nothing model-influenced. This is the
// fixed-command guarantee behind C5/C8.
func TestProcessLaunchesVerifiedPath(t *testing.T) {
	conn := newFakeConn(echoStub())
	fl := &fakeLauncher{conn: conn}
	h := NewProcessHandler(fl, okResolver{args: []string{"serve", "--stdio"}})
	m := Mount{ID: "ext-1", Name: "token", Surface: SurfaceProcess, Block: json.RawMessage(`{}`)}
	if err := h.OnLoad(context.Background(), m); err != nil {
		t.Fatalf("OnLoad: %v", err)
	}
	t.Cleanup(func() { _ = h.OnUnload(context.Background(), m.ID) })
	req := fl.lastRequest()
	if req.Path != "/verified/bin" {
		t.Fatalf("launched path = %q, want the resolver's verified path", req.Path)
	}
	if strings.Join(req.Args, " ") != "serve --stdio" {
		t.Fatalf("launched args = %v, want the resolver's fixed args", req.Args)
	}
}

// TestDevResolverRefusesWhenDisabled proves a dev source is refused unless dev mode is
// explicitly enabled, so unsigned code never runs in a normal run (C8, fail-closed).
func TestDevResolverRefusesWhenDisabled(t *testing.T) {
	block := ProcessBlock{Dev: &DevSource{Path: "/tmp/whatever"}}
	if _, _, err := (DevResolver{Enabled: false}).Resolve(context.Background(), "token", block); err == nil {
		t.Fatal("expected a dev source to be refused when dev mode is off")
	}
}

// TestDevResolverRefusesReleaseSource proves the dev resolver never resolves a released
// (signed-distribution) source: that path belongs to the verifying resolver, so the dev
// resolver fails closed rather than launching a release binary unverified.
func TestDevResolverRefusesReleaseSource(t *testing.T) {
	block := ProcessBlock{Release: &ReleaseSource{Asset: "token", Version: "1.0.0"}}
	if _, _, err := (DevResolver{Enabled: true}).Resolve(context.Background(), "token", block); err == nil {
		t.Fatal("expected the dev resolver to refuse a released source")
	}
}

// TestDevResolverRefusesRelativePath proves a dev path must be absolute, so a relative or
// PATH-resolved name cannot redirect the launch.
func TestDevResolverRefusesRelativePath(t *testing.T) {
	block := ProcessBlock{Dev: &DevSource{Path: "relative/bin"}}
	if _, _, err := (DevResolver{Enabled: true}).Resolve(context.Background(), "token", block); err == nil {
		t.Fatal("expected a relative dev path to be refused")
	}
}
