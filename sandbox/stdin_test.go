package sandbox

import (
	"context"
	"strings"
	"testing"
)

// TestExecStdinDeliversInputOffCommandLine proves a command receives Command.Stdin on its
// standard input through the confined execution path, and that the value reaches the
// command without being placed on the command line. This is the mechanism that lets a
// credential reach a tool that reads one on stdin (a `secrets import`) without the value
// ever appearing in argv, where another process could read it, or in any rendered command.
func TestExecStdinDeliversInputOffCommandLine(t *testing.T) {
	sb, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	const secret = "s3cr3t-value-on-stdin"
	// `sort` copies a single line of standard input to standard output unchanged on every
	// platform, so it is a portable echo of whatever the pipe delivered.
	res, err := sb.Exec(context.Background(), Command{Line: "sort", Stdin: []byte(secret + "\n")})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Output, secret) {
		t.Fatalf("stdin was not delivered to the command; output = %q", res.Output)
	}
}

// TestExecNoStdinIsUnaffected proves a command with no Stdin runs exactly as before: the
// optional pipe is only set up when there is input to deliver.
func TestExecNoStdinIsUnaffected(t *testing.T) {
	sb, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()

	res, err := sb.Exec(context.Background(), Command{Line: "echo ready"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Output, "ready") {
		t.Fatalf("command without stdin did not run as expected; output = %q", res.Output)
	}
}
