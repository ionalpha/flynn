package externagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/bindguard"
	"github.com/ionalpha/flynn/mcp"
	"github.com/ionalpha/flynn/secret"
)

// bridgeForwarder is a Spawner whose child has its own network namespace and so cannot
// reach the host-loopback bridge. ForwardBridge returns the URL the child should dial
// and the host address to forward it to (empty when no forward is needed). A spawner
// without it leaves the child pointed at the host bridge, which suits a shared stack.
type bridgeForwarder interface {
	ForwardBridge(hostURL string) (childURL, forwardTo string)
}

const defaultBridgeName = "flynn"

// defaultTokenEnv carries the bridge's bearer token, keeping it out of the process
// table.
const defaultTokenEnv = "FLYNN_MCP_TOKEN" //nolint:gosec // G101: this is the env var NAME the token is read from, not a credential

// Runner drives one episode: it hosts the loopback MCP bridge on a local
// streamable-HTTP port, spawns the CLI, projects its output to events, and tears
// everything down. The bridge serves on the governed context passed to Run, so every
// tool call is admitted, contained, braked and recorded at the dispatch waist, and
// cancelling that context refuses further calls and kills the subprocess.
type Runner struct {
	adapter Adapter
	server  *mcp.Server
	spawner Spawner
	report  func(Event)
	probes  []Probe
}

// NewRunner builds a runner hosting server as the bridge and spawning through spawner.
// report receives each projected event; nil drops them.
func NewRunner(adapter Adapter, server *mcp.Server, spawner Spawner, report func(Event)) *Runner {
	return &Runner{adapter: adapter, server: server, spawner: spawner, report: report}
}

// WithProbes sets the conformance probes to watch for. Their instructions must also go
// into Episode.Probes, or the probe measures only the model's guesswork.
func (r *Runner) WithProbes(probes []Probe) *Runner {
	r.probes = probes
	return r
}

// Result is the outcome of one episode. Reasoning is absent from the tier tally because
// an external harness keeps it unobservable; the record names that gap separately.
type Result struct {
	Text     string
	Usage    Usage
	Failed   bool
	Err      string
	Terminal bool
	Tiers    map[Tier]int
	// Conformance is the evidence of whether the run's instructions were followed.
	Conformance ConformanceReport
	// Session is the conversation id the CLI announced, if any. Passing it on a later
	// Episode keeps the harness's context across turns.
	Session string
}

// Run hosts the bridge, runs one episode, and returns its Result. ctx must carry the
// run's grant and brake so bridged calls are governed; cancelling it kills the
// subprocess. The error is non-nil only when the episode cannot start or be hosted; an
// episode the CLI reports as failed returns Failed on the Result.
func (r *Runner) Run(ctx context.Context, ep Episode) (Result, error) {
	// Read before anything binds or spawns, so a provider whose native surface cannot
	// be removed never runs. Terminal, because it is an integration gap; the reason
	// names what could not be stripped.
	if why := r.adapter.Lockdown().Refusal(r.adapter.Name()); why != "" {
		return Result{}, fault.New(fault.Terminal, "externagent_lockdown", why)
	}

	// Loopback only, through the bind-safe gate, so the endpoint stays local.
	ln, err := bindguard.Listen("tcp", "127.0.0.1:0", bindguard.Loopback())
	if err != nil {
		return Result{}, fault.Wrap(fault.Transient, "externagent_bridge_listen", err)
	}
	token, err := ids.Token()
	if err != nil {
		_ = ln.Close()
		return Result{}, fault.Wrap(fault.Terminal, "externagent_bridge_token", err)
	}

	srv := &http.Server{
		Handler:           r.server.HTTPHandler(ctx, token),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	port := ln.Addr().(*net.TCPAddr).Port
	// A child in its own network namespace gets an in-namespace address the spawner
	// forwards here; otherwise it dials this address directly.
	hostURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	childURL, forwardTo := hostURL, ""
	if fw, ok := r.spawner.(bridgeForwarder); ok {
		childURL, forwardTo = fw.ForwardBridge(hostURL)
	}
	ep.Bridge = Bridge{
		Name:      defaultBridgeName,
		URL:       childURL,
		ForwardTo: forwardTo,
		Token:     token,
		TokenEnv:  defaultTokenEnv,
	}

	inv, err := r.adapter.Command(ep)
	if err != nil {
		return Result{}, fault.Wrap(fault.Terminal, "externagent_command", err)
	}

	proc, err := r.spawner.Start(ctx, ep, inv)
	if err != nil {
		return Result{}, fault.Wrap(fault.Terminal, "externagent_spawn", err)
	}

	result := Result{Tiers: map[Tier]int{}}
	watch := newConformanceWatch(r.probes, ep.Bridge.Name)
	scanner := bufio.NewScanner(proc.Stdout())
	// An echoed tool result can be large, so the line limit is 4 MiB instead of the
	// default 64 KiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		evs, perr := r.adapter.Parse(scanner.Bytes())
		if perr != nil {
			continue
		}
		for _, ev := range evs {
			ev.Raw = redact(ev.Raw, token)
			r.emit(ev)
			result.absorb(ev)
			watch.observe(ev)
		}
	}
	waitErr := proc.Wait()
	result.Conformance = watch.report()

	// The message file is more reliable than reconstructing the reply from events.
	if inv.LastMessageFile != "" {
		path := inv.LastMessageFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(ep.Workdir, inv.LastMessageFile)
		}
		// Not caller-tainted: the workdir joined with the adapter's fixed filename.
		if b, rerr := os.ReadFile(path); rerr == nil && len(b) > 0 { //nolint:gosec // G304: path is workdir + adapter's fixed filename
			result.Text = string(b)
		}
	}

	// A cancelled context is a halt, recorded as terminal whatever the exit status.
	if ctx.Err() != nil {
		result.Failed = true
		result.Terminal = true
		if result.Err == "" {
			result.Err = "episode halted: " + ctx.Err().Error()
		}
		return result, nil
	}
	// A non-zero exit with no error event is still a failure.
	if waitErr != nil && !result.Failed {
		result.Failed = true
		result.Err = waitErr.Error()
	}
	return result, nil
}

// redact removes the bridge's bearer token from a harness line. The line is kept
// verbatim in a signed, durable record, so anything the CLI echoes (its argv, its
// environment, an error quoting its header) would carry the token. The token is the one
// secret this run mints, so it is the one the runner can remove with certainty. It is a
// URL-safe id with no JSON metacharacter, so the line stays valid JSON.
func redact(raw json.RawMessage, token string) json.RawMessage {
	if len(raw) == 0 || token == "" {
		return raw
	}
	return bytes.ReplaceAll(raw, []byte(token), []byte(secret.Redacted))
}

func (r *Runner) emit(ev Event) {
	if r.report != nil {
		r.report(ev)
	}
}

// absorb folds one event into the result. Attested text is the fallback when no
// message file is written.
func (res *Result) absorb(ev Event) {
	res.Tiers[ev.Tier]++
	// Both harnesses repeat the conversation id on many lines, so it is read from any
	// event kind.
	if ev.Session != "" {
		res.Session = ev.Session
	}
	switch ev.Kind {
	case EventText:
		if ev.Text != "" {
			res.Text = ev.Text
		}
	case EventUsage, EventDone:
		if ev.Usage != (Usage{}) {
			res.Usage = ev.Usage
		}
	case EventError:
		res.Failed = true
		res.Err = ev.Err
		res.Terminal = res.Terminal || ev.Terminal
	case EventProgress, EventBridgeCall, EventNativeCommand:
		// No result of their own; already tallied, and counted for steering by the
		// conformance watch.
	}
}
