package externagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/mcp"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/tools"
)

// fakeProc is a scripted episode process: its stdout is a pipe a launcher goroutine
// writes events to, and Wait blocks until that goroutine finishes.
type fakeProc struct {
	pr   *io.PipeReader
	wait chan error
}

func (f *fakeProc) Stdout() io.Reader { return f.pr }
func (f *fakeProc) Wait() error       { return <-f.wait }

// bridgeClient is a minimal MCP-over-HTTP client: it does the initialize handshake
// and one tools/call against the bridge, returning whether the call succeeded.
func bridgeClient(b Bridge, tool, args string) (bool, error) {
	post := func(body string) (map[string]json.RawMessage, error) {
		req, _ := http.NewRequest(http.MethodPost, b.URL, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+b.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Result map[string]json.RawMessage `json:"result"`
		}
		return out.Result, json.NewDecoder(resp.Body).Decode(&out)
	}
	if _, err := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`); err != nil {
		return false, err
	}
	res, err := post(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args))
	if err != nil {
		return false, err
	}
	var isErr bool
	if raw, ok := res["isError"]; ok {
		_ = json.Unmarshal(raw, &isErr)
	}
	return !isErr, nil
}

// governedBridge builds a real bridge server over the default toolset in workdir,
// governed by a grant, plus the context that binds the grant.
func governedBridge(t *testing.T, workdir string, grant capability.Grant) (*mcp.Server, context.Context) {
	t.Helper()
	sb, err := sandbox.NewLocal(workdir, sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	d := dispatch.New(dispatch.WithAdmitter(capability.Admitter{}))
	srv := mcp.NewServer(d, tools.New(sb).Tools())
	ctx := capability.Into(context.Background(), grant)
	return srv, ctx
}

// scriptSpawner returns a fakeSpawner whose episode goroutine runs body against the
// pipe writer, then closes it. body is the whole episode: it may call the bridge and
// write event lines.
func scriptSpawner(body func(ep Episode, inv Invocation, pw *io.PipeWriter)) fakeSpawner {
	return fakeSpawner{start: func(_ context.Context, ep Episode, inv Invocation) (Process, error) {
		pr, pw := io.Pipe()
		wait := make(chan error, 1)
		go func() {
			body(ep, inv, pw)
			_ = pw.Close()
			wait <- nil
		}()
		return &fakeProc{pr: pr, wait: wait}, nil
	}}
}

// TestRunnerBridgesEffectsThroughWaist is the end-to-end property: a spawned episode
// process calls a tool over the loopback bridge, and the call reaches the real tool
// through the dispatch waist, so the effect lands in the workspace. The runner also
// projects the episode's event stream and reads the final message from the file.
func TestRunnerBridgesEffectsThroughWaist(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.NewGrant("write", "read"))

	spawner := scriptSpawner(func(ep Episode, inv Invocation, pw *io.PipeWriter) {
		// The external process writes to the workspace through the bridge, not natively.
		ok, err := bridgeClient(ep.Bridge, "write", `{"path":"bridged.txt","content":"via-bridge"}`)
		if err != nil || !ok {
			_, _ = fmt.Fprintf(pw, `{"type":"error","message":"bridge call failed: %v"}`+"\n", err)
		}
		_, _ = fmt.Fprintln(pw, `{"type":"thread.started","thread_id":"x"}`)
		_, _ = fmt.Fprintln(pw, `{"type":"item.completed","item":{"type":"agent_message","text":"streamed"}}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":3}}`)
		_ = os.WriteFile(filepath.Join(ep.Workdir, inv.LastMessageFile), []byte("final message"), 0o644)
	})

	var events []Event
	r := NewRunner(NewCodex("", nil), srv, spawner, func(e Event) { events = append(events, e) })

	res, err := r.Run(ctx, Episode{Input: "write a file", Workdir: workdir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The bridged effect landed in the workspace through the waist.
	b, rerr := os.ReadFile(filepath.Join(workdir, "bridged.txt"))
	if rerr != nil || string(b) != "via-bridge" {
		t.Fatalf("bridged write did not land: %v / %q", rerr, string(b))
	}
	// The final message came from the file; usage came from the stream.
	if res.Text != "final message" {
		t.Errorf("final message not read from file: %q", res.Text)
	}
	if res.Usage != (Usage{InputTokens: 7, OutputTokens: 3}) {
		t.Errorf("usage not projected: %+v", res.Usage)
	}
	if res.Failed {
		t.Errorf("episode should have succeeded: %+v", res)
	}
	if res.Tiers[TierAttested] == 0 {
		t.Errorf("expected attested events in the tally: %+v", res.Tiers)
	}
	if len(events) == 0 {
		t.Errorf("no events forwarded to the reporter")
	}
}

// TestRunnerDeniedBridgeCallIsRefused proves the bridge still governs: a tool the
// grant does not permit is refused even though the episode process tried it, and no
// effect lands.
func TestRunnerDeniedBridgeCallIsRefused(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.NewGrant("read")) // no write

	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		ok, _ := bridgeClient(ep.Bridge, "write", `{"path":"nope.txt","content":"x"}`)
		_, _ = fmt.Fprintf(pw, `{"type":"item.completed","item":{"type":"agent_message","text":"denied=%v"}}`+"\n", !ok)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	r := NewRunner(NewCodex("", nil), srv, spawner, nil)

	if _, err := r.Run(ctx, Episode{Workdir: workdir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "nope.txt")); !os.IsNotExist(err) {
		t.Errorf("denied write should not have created a file")
	}
}

// TestRunnerHaltKillsEpisode proves a run-level halt (context cancelled) ends the
// episode as a terminal failure and stops the subprocess.
func TestRunnerHaltKillsEpisode(t *testing.T) {
	workdir := t.TempDir()
	srv, base := governedBridge(t, workdir, capability.AllowAll())
	ctx, cancel := context.WithCancel(base)

	entered := make(chan struct{})
	spawner := fakeSpawner{start: func(pctx context.Context, _ Episode, _ Invocation) (Process, error) {
		pr, pw := io.Pipe()
		wait := make(chan error, 1)
		go func() {
			close(entered)
			<-pctx.Done() // the process runs until the run is halted
			_ = pw.Close()
			wait <- pctx.Err()
		}()
		return &fakeProc{pr: pr, wait: wait}, nil
	}}
	r := NewRunner(NewCodex("", nil), srv, spawner, nil)

	done := make(chan Result, 1)
	go func() {
		res, _ := r.Run(ctx, Episode{Workdir: workdir})
		done <- res
	}()
	<-entered
	cancel()
	select {
	case res := <-done:
		if !res.Failed || !res.Terminal {
			t.Errorf("halted episode should be a terminal failure: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("halt did not end the episode")
	}
}

// TestRunnerRedactsBridgeTokenFromRawLines proves the bearer token the run mints for the
// bridge never reaches the record. The harness's lines are kept verbatim in a signed,
// durable artifact, and a CLI that echoes its own environment or quotes the header it
// sent would otherwise write a live credential into it.
func TestRunnerRedactsBridgeTokenFromRawLines(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.NewGrant("read"))

	spawner := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		// A CLI echoing the credential it was handed, in the shape an error line would.
		_, _ = fmt.Fprintf(pw, `{"type":"error","message":"auth failed with bearer %s"}`+"\n", ep.Bridge.Token)
	})

	var events []Event
	r := NewRunner(NewCodex("", nil), srv, spawner, func(e Event) { events = append(events, e) })
	if _, err := r.Run(ctx, Episode{Input: "go", Workdir: workdir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events projected")
	}
	for _, ev := range events {
		if bytes.Contains(ev.Raw, []byte("bearer ")) && !bytes.Contains(ev.Raw, []byte(secret.Redacted)) {
			t.Fatalf("the bridge token reached the record: %s", ev.Raw)
		}
	}
	// The rest of the line survives: redaction replaces the secret, it does not drop the
	// harness's account of what went wrong.
	if !bytes.Contains(events[0].Raw, []byte("auth failed with bearer "+secret.Redacted)) {
		t.Errorf("raw line = %s, want the token replaced in place", events[0].Raw)
	}
	// A valid JSON line stays valid JSON after redaction, so the record's raw payload can
	// still be parsed by whatever reads it.
	if !json.Valid(events[0].Raw) {
		t.Errorf("redaction broke the line's JSON: %s", events[0].Raw)
	}
}

// TestRedactLeavesCleanLinesAlone proves redaction is a no-op when there is nothing to
// remove: a line without the token, and an episode whose token was never minted, come
// through byte-identical.
func TestRedactLeavesCleanLinesAlone(t *testing.T) {
	line := json.RawMessage(`{"type":"turn.completed"}`)
	if got := redact(line, "tok"); string(got) != string(line) {
		t.Errorf("redact rewrote a clean line: %s", got)
	}
	if got := redact(line, ""); string(got) != string(line) {
		t.Errorf("redact with no token rewrote the line: %s", got)
	}
	if got := redact(nil, "tok"); got != nil {
		t.Errorf("redact invented a line out of nothing: %s", got)
	}
}
