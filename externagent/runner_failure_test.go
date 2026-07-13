package externagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/fault"
)

// stubAdapter is an Adapter whose Command and Parse are scripted, so the runner's handling
// of an adapter that cannot build an invocation, or cannot read a line, is exercised without
// a real CLI. Unset behaviour falls through to the codex adapter, which both real adapters
// share the shape of.
type stubAdapter struct {
	*Codex
	command func(ep Episode) (Invocation, error)
	parse   func(line []byte) ([]Event, error)
}

func (s stubAdapter) Command(ep Episode) (Invocation, error) {
	if s.command != nil {
		return s.command(ep)
	}
	return s.Codex.Command(ep)
}

func (s stubAdapter) Parse(line []byte) ([]Event, error) {
	if s.parse != nil {
		return s.parse(line)
	}
	return s.Codex.Parse(line)
}

// TestRunnerReportsAnUnbuildableInvocation proves an adapter that cannot build the episode's
// argv fails the run as terminal rather than launching something half-configured. A partly
// built invocation is exactly the one whose lockdown flags might be missing, so it must
// never be spawned.
func TestRunnerReportsAnUnbuildableInvocation(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.AllowAll())

	spawner := fakeSpawner{start: func(context.Context, Episode, Invocation) (Process, error) {
		return nil, errors.New("nothing may be spawned from an invocation that could not be built")
	}}
	adapter := stubAdapter{Codex: NewCodex("", nil), command: func(Episode) (Invocation, error) {
		return Invocation{}, errors.New("the bridge config could not be encoded")
	}}
	r := NewRunner(adapter, srv, spawner, nil)

	_, err := r.Run(ctx, Episode{Workdir: workdir})
	if err == nil {
		t.Fatal("an invocation that could not be built must fail the run")
	}
	if fault.Classify(err) != fault.Terminal {
		t.Errorf("a bad invocation is terminal, not worth a retry: class = %v", fault.Classify(err))
	}
}

// TestRunnerReportsAFailureToSpawn proves an episode whose subprocess never started fails the
// run rather than returning an empty Result that a caller would read as a turn that ran and
// said nothing.
func TestRunnerReportsAFailureToSpawn(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.AllowAll())

	spawner := fakeSpawner{start: func(context.Context, Episode, Invocation) (Process, error) {
		return nil, errors.New("host containment is below the required floor")
	}}
	r := NewRunner(NewCodex("", nil), srv, spawner, nil)

	_, err := r.Run(ctx, Episode{Workdir: workdir})
	if err == nil {
		t.Fatal("an episode that never launched must fail the run")
	}
	if !strings.Contains(err.Error(), "containment") {
		t.Errorf("the spawner's reason must survive: %v", err)
	}
}

// TestRunnerSkipsALineItCannotProject proves a line the adapter cannot read is skipped and
// the episode carries on. The CLI's stream is not the run's to control: a build that emits an
// unfamiliar or malformed line must not end an episode whose effects are still enforced and
// recorded at the waist.
func TestRunnerSkipsALineItCannotProject(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.AllowAll())

	adapter := stubAdapter{Codex: NewCodex("", nil), parse: func(line []byte) ([]Event, error) {
		if strings.Contains(string(line), "unreadable") {
			return nil, errors.New("this line is not projectable")
		}
		return NewCodex("", nil).Parse(line)
	}}
	spawner := scriptSpawner(func(_ Episode, _ Invocation, pw *io.PipeWriter) {
		_, _ = fmt.Fprintln(pw, `{"type":"unreadable"}`)
		_, _ = fmt.Fprintln(pw, `{"type":"item.completed","item":{"type":"agent_message","text":"carried on"}}`)
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})

	var events []Event
	r := NewRunner(adapter, srv, spawner, func(e Event) { events = append(events, e) })
	res, err := r.Run(ctx, Episode{Workdir: workdir})
	if err != nil {
		t.Fatalf("an unprojectable line must not fail the episode: %v", err)
	}
	if res.Failed {
		t.Errorf("the episode should have completed: %+v", res)
	}
	if res.Text != "carried on" {
		t.Errorf("the lines after the unreadable one were dropped: %q", res.Text)
	}
	for _, ev := range events {
		if strings.Contains(string(ev.Raw), "unreadable") {
			t.Errorf("an unprojectable line reached the record as an event: %s", ev.Raw)
		}
	}
}

// TestRunnerRecordsANonZeroExitWithNoErrorEvent proves an episode whose CLI died without
// saying why is still recorded as failed. A harness that crashes mid-turn projects no error
// event, so reading only the stream would settle the goal on a turn that never finished.
func TestRunnerRecordsANonZeroExitWithNoErrorEvent(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.AllowAll())

	spawner := fakeSpawner{start: func(_ context.Context, _ Episode, _ Invocation) (Process, error) {
		pr, pw := io.Pipe()
		wait := make(chan error, 1)
		go func() {
			// A turn's worth of output, then a crash: not one error event on the stream.
			_, _ = fmt.Fprintln(pw, `{"type":"item.completed","item":{"type":"agent_message","text":"half an answer"}}`)
			_ = pw.Close()
			wait <- errors.New("exit status 134")
		}()
		return &fakeProc{pr: pr, wait: wait}, nil
	}}
	r := NewRunner(NewCodex("", nil), srv, spawner, nil)

	res, err := r.Run(ctx, Episode{Workdir: workdir})
	if err != nil {
		t.Fatalf("a crashed episode is a completed Run with a failed Result: %v", err)
	}
	if !res.Failed {
		t.Fatal("an episode whose CLI exited non-zero must be recorded as failed")
	}
	if !strings.Contains(res.Err, "exit status 134") {
		t.Errorf("the exit failure must be carried as the reason: %q", res.Err)
	}
	// The lines it did produce before dying are still projected, so the record holds the
	// harness's account of what it was doing when it went.
	if res.Text != "half an answer" {
		t.Errorf("the lines before the crash were dropped: %q", res.Text)
	}
}

// TestRunnerErrorEventOutranksTheExitCode proves a CLI that reported why it failed keeps its
// own reason. The exit code says only that it died; the error event says what went wrong, and
// that is the reason a reader needs and a retry decision rests on.
func TestRunnerErrorEventOutranksTheExitCode(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.AllowAll())

	spawner := fakeSpawner{start: func(_ context.Context, _ Episode, _ Invocation) (Process, error) {
		pr, pw := io.Pipe()
		wait := make(chan error, 1)
		go func() {
			_, _ = fmt.Fprintln(pw, `{"type":"turn.failed","error":{"message":"unauthorized: the subscription expired"}}`)
			_ = pw.Close()
			wait <- errors.New("exit status 1")
		}()
		return &fakeProc{pr: pr, wait: wait}, nil
	}}
	r := NewRunner(NewCodex("", nil), srv, spawner, nil)

	res, err := r.Run(ctx, Episode{Workdir: workdir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Failed || !res.Terminal {
		t.Fatalf("an auth failure is a terminal failure: %+v", res)
	}
	if !strings.Contains(res.Err, "unauthorized") {
		t.Errorf("the CLI's own reason was replaced by the exit code: %q", res.Err)
	}
}

// forwardingSpawner is a spawner whose child runs in its own network namespace: it cannot
// reach the host loopback, so the bridge must be forwarded in. It reports the in-namespace
// address the child should dial and the host address the sandbox forwards it to.
type forwardingSpawner struct {
	fakeSpawner
	childURL, forwardTo string
}

func (f forwardingSpawner) ForwardBridge(string) (string, string) {
	return f.childURL, f.forwardTo
}

// TestRunnerHandsTheChildAnAddressItCanReach is what makes the bridge reachable from a child
// confined to its own network namespace. The host loopback is not the child's loopback, so
// the spawner reports an in-namespace address and the host address to forward it to, and the
// episode must be configured with the address the child can actually dial. Handing it the
// host URL instead would leave the harness with no governed tools at all.
func TestRunnerHandsTheChildAnAddressItCanReach(t *testing.T) {
	workdir := t.TempDir()
	srv, ctx := governedBridge(t, workdir, capability.AllowAll())

	const childURL = "http://10.0.2.2:9/mcp"
	var saw Episode
	base := scriptSpawner(func(ep Episode, _ Invocation, pw *io.PipeWriter) {
		saw = ep
		_, _ = fmt.Fprintln(pw, `{"type":"turn.completed"}`)
	})
	spawner := forwardingSpawner{fakeSpawner: base, childURL: childURL, forwardTo: "127.0.0.1:9"}

	if _, err := NewRunner(NewCodex("", nil), srv, spawner, nil).Run(ctx, Episode{Workdir: workdir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if saw.Bridge.URL != childURL {
		t.Errorf("the child was pointed at %q, want the in-namespace address %q it can reach", saw.Bridge.URL, childURL)
	}
	if saw.Bridge.ForwardTo != "127.0.0.1:9" {
		t.Errorf("the spawner was not told what to forward the child's address to: %q", saw.Bridge.ForwardTo)
	}
	if saw.Bridge.Token == "" || saw.Bridge.TokenEnv != defaultTokenEnv {
		t.Errorf("the bearer token must still be minted and named for the environment: %+v", saw.Bridge)
	}
}
