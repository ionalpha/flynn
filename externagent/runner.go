package externagent

import (
	"bufio"
	"context"
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
)

// defaultBridgeName is the MCP server name the loopback bridge is registered under.
const defaultBridgeName = "flynn"

// defaultTokenEnv is the environment variable the external CLI reads the bridge's
// bearer token from, so the token stays out of the process table.
const defaultTokenEnv = "FLYNN_MCP_TOKEN" //nolint:gosec // G101: this is the env var NAME the token is read from, not a credential

// Runner drives one external-agent episode end to end: it hosts the loopback MCP
// bridge over a local streamable-HTTP port, spawns the CLI constrained to route its
// effects through that bridge, projects the CLI's output stream to typed events, and
// tears everything down. The bridge is served on the governed context passed to Run,
// so every tool call the CLI makes is admitted, contained, braked, and recorded at
// the dispatch waist; cancelling that context (a halt or shutdown) both refuses
// further tool calls and kills the subprocess.
type Runner struct {
	adapter Adapter
	server  *mcp.Server
	spawner Spawner
	report  func(Event)
}

// NewRunner builds a runner that drives adapter, hosting server as the bridge,
// spawning the episode subprocess through spawner (the sandbox-confined process
// host), and forwarding each projected event to report (nil drops them).
func NewRunner(adapter Adapter, server *mcp.Server, spawner Spawner, report func(Event)) *Runner {
	return &Runner{adapter: adapter, server: server, spawner: spawner, report: report}
}

// Result is the outcome of one episode: the final assistant message, the token
// usage the CLI reported, whether it failed and why, and the provenance-tier tally
// of the projected events. Reasoning is not in the tally because it is unobservable
// with an external harness in the loop; the record names that gap separately.
type Result struct {
	Text     string
	Usage    Usage
	Failed   bool
	Err      string
	Terminal bool
	Tiers    map[Tier]int
}

// Run hosts the bridge, runs one episode of ep, and returns its Result. ctx must
// carry the run's governance bindings (the grant and the brake) so bridged calls are
// governed; cancelling ctx halts the episode and kills the subprocess. Run returns a
// non-nil error only on a failure to start or host the episode (a bad invocation, a
// bridge that will not bind); an episode that runs and the CLI reports as failed is a
// completed Run with Failed set on the Result.
func (r *Runner) Run(ctx context.Context, ep Episode) (Result, error) {
	// The bridge binds a loopback port through the bind-safe gate, never a raw or
	// non-loopback listen, so the endpoint an external process reaches stays local.
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
	ep.Bridge = Bridge{
		Name:     defaultBridgeName,
		URL:      fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		Token:    token,
		TokenEnv: defaultTokenEnv,
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
	scanner := bufio.NewScanner(proc.Stdout())
	// An episode line (a tool result echoed in the stream) can be large; raise the
	// scanner's line ceiling well above the default 64KiB.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		evs, perr := r.adapter.Parse(scanner.Bytes())
		if perr != nil {
			continue
		}
		for _, ev := range evs {
			r.emit(ev)
			result.absorb(ev)
		}
	}
	waitErr := proc.Wait()

	// The final assistant message is read from the file the CLI wrote, which is more
	// reliable than reconstructing it from the event stream.
	if inv.LastMessageFile != "" {
		path := inv.LastMessageFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(ep.Workdir, inv.LastMessageFile)
		}
		// The path is the episode workdir joined with the adapter's fixed filename, not
		// caller-tainted input.
		if b, rerr := os.ReadFile(path); rerr == nil && len(b) > 0 { //nolint:gosec // G304: path is workdir + adapter's fixed filename
			result.Text = string(b)
		}
	}

	// A cancelled context is a halt: report it as a terminal failure regardless of how
	// the process exited, so a killed episode is recorded as halted rather than done.
	if ctx.Err() != nil {
		result.Failed = true
		result.Terminal = true
		if result.Err == "" {
			result.Err = "episode halted: " + ctx.Err().Error()
		}
		return result, nil
	}
	// A non-zero exit with no error event projected is still a failure worth recording.
	if waitErr != nil && !result.Failed {
		result.Failed = true
		result.Err = waitErr.Error()
	}
	return result, nil
}

// emit forwards an event to the reporter when one is set.
func (r *Runner) emit(ev Event) {
	if r.report != nil {
		r.report(ev)
	}
}

// absorb folds one event into the running result: the latest usage, the final text
// (an attested text event is a fallback when no message file is written), any failure,
// and the provenance-tier tally.
func (res *Result) absorb(ev Event) {
	res.Tiers[ev.Tier]++
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
	case EventProgress:
		// A progress event carries no result of its own; it is counted in the tier tally
		// above and forwarded to the reporter for the live trace.
	}
}
