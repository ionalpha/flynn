package externagent

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
)

// execSpawner is a real os/exec-backed Spawner used by tests to exercise detection
// and (where a CLI is present) a live episode. Test files are exempt from the
// no-direct-exec rule; in production a Spawner is backed by the sandbox-confined
// process host, not this.
type execSpawner struct{}

func (execSpawner) Probe(ctx context.Context, path string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	return string(out), err
}

func (execSpawner) Start(ctx context.Context, ep Episode, inv Invocation) (Process, error) {
	cmd := exec.CommandContext(ctx, inv.Path, inv.Args...)
	cmd.Dir = ep.Workdir
	cmd.Env = append(os.Environ(), inv.Env...)
	if inv.Stdin != "" {
		cmd.Stdin = strings.NewReader(inv.Stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd, stdout: stdout}, nil
}

type execProc struct {
	cmd    *exec.Cmd
	stdout io.Reader
}

func (p *execProc) Stdout() io.Reader { return p.stdout }
func (p *execProc) Wait() error       { return p.cmd.Wait() }

// fakeSpawner drives an episode with a scripted Start, so the runner's lifecycle is
// exercised without a real CLI.
type fakeSpawner struct {
	start func(ctx context.Context, ep Episode, inv Invocation) (Process, error)
}

func (f fakeSpawner) Probe(context.Context, string, ...string) (string, error) { return "", nil }

func (f fakeSpawner) Start(ctx context.Context, ep Episode, inv Invocation) (Process, error) {
	return f.start(ctx, ep, inv)
}
