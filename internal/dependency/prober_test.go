package dependency

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

// stubSandbox records the command it was asked to run and returns a scripted result, so a
// test can assert exactly what the version probe put on the command line without running
// anything. It implements the parts of sandbox.Sandbox the prober uses; the rest are no-ops.
type stubSandbox struct {
	line  string   // the last command line
	lines []string // every command line, in order
	res   sandbox.ExecResult
	err   error
}

func (s *stubSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	s.line = cmd.Line
	s.lines = append(s.lines, cmd.Line)
	return s.res, s.err
}
func (s *stubSandbox) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (s *stubSandbox) WriteFile(context.Context, string, []byte) error  { return nil }
func (s *stubSandbox) Glob(context.Context, string) ([]string, error)   { return nil, nil }
func (s *stubSandbox) Walk(context.Context, string) ([]string, error)   { return nil, nil }
func (s *stubSandbox) Close() error                                     { return nil }

// TestSandboxProbeRunsThroughTheSandbox proves the probe runs "name args..." through the
// sandbox boundary rather than spawning a process, and returns the program's output
// trimmed, whatever version arguments the spec carries.
func TestSandboxProbeRunsThroughTheSandbox(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"flyctl", []string{"version"}, "flyctl version"},
		{"flyctl", []string{"--version", "--json"}, "flyctl --version --json"},
		{"flyctl", nil, "flyctl"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			sb := &stubSandbox{res: sandbox.ExecResult{ExitCode: 0, Output: "  flyctl v0.4.61\n"}}
			out, err := NewSandboxProber(sb).Probe(context.Background(), tc.name, tc.args)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if sb.line != tc.want {
				t.Fatalf("probe command line is %q, want %q", sb.line, tc.want)
			}
			if out != "flyctl v0.4.61" {
				t.Fatalf("probe output not trimmed and returned: %q", out)
			}
		})
	}
}

// TestSandboxProbeTreatsFailureAsNotPresent proves every way a probe can fail (no sandbox
// wired, the sandbox could not run the command, the program exited non-zero) is reported as
// an error and never as an empty version the engine would parse as a usable install.
func TestSandboxProbeTreatsFailureAsNotPresent(t *testing.T) {
	for _, tc := range []struct {
		name string
		sb   sandbox.Sandbox
	}{
		{"no sandbox", nil},
		{"sandbox unavailable", &stubSandbox{err: errors.New("sandbox unavailable")}},
		{"non-zero exit", &stubSandbox{res: sandbox.ExecResult{ExitCode: 127, Output: "command not found"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NewSandboxProber(tc.sb).Probe(context.Background(), "flyctl", []string{"version"})
			if err == nil {
				t.Fatal("a failed probe must be an error, not a usable version")
			}
			if out != "" {
				t.Fatalf("a failed probe must return no output, got %q", out)
			}
		})
	}
}

// TestSandboxProbeSatisfiesTheProberPort proves the sandbox prober is what the manager
// accepts as its version-probe boundary, and that a manager wired with it detects a present
// program end to end without a download.
func TestSandboxProbeSatisfiesTheProberPort(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, "https://127.0.0.1:1/never", "00", 1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	sb := &stubSandbox{res: sandbox.ExecResult{Output: "flyctl v0.4.61 linux/amd64"}}
	m := NewManager(s, nil, t.TempDir(), WithProber(NewSandboxProber(sb)), WithPlatform("linux", "amd64"))

	got, err := m.Resolve(ctx, "flyctl")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != SourceSystem || got.Version != "0.4.61" {
		t.Fatalf("expected the probed system binary, got %+v", got)
	}
	if len(sb.lines) == 0 || sb.lines[0] != "flyctl version" {
		t.Fatalf("the manager did not probe the preferred binary through the sandbox: %v", sb.lines)
	}
}
