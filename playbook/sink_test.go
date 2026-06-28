package playbook

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/secret"
)

// fixedSource resolves any reference to one value, standing in for the vault.
type fixedSource struct {
	val secret.Text
	err error
}

func (s fixedSource) Lookup(context.Context, string) (secret.Text, error) { return s.val, s.err }

// recordingSandbox records the last command it was asked to run, so a test can assert what
// reached the command line and what was delivered on standard input. It implements the
// parts of sandbox.Sandbox the sink uses; the rest are unused no-ops.
type recordingSandbox struct {
	line  string
	stdin []byte
	res   sandbox.ExecResult
}

func (s *recordingSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	s.line, s.stdin = cmd.Line, cmd.Stdin
	return s.res, nil
}
func (s *recordingSandbox) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (s *recordingSandbox) WriteFile(context.Context, string, []byte) error  { return nil }
func (s *recordingSandbox) Glob(context.Context, string) ([]string, error)   { return nil, nil }
func (s *recordingSandbox) Walk(context.Context, string) ([]string, error)   { return nil, nil }
func (s *recordingSandbox) Close() error                                     { return nil }

// TestFlySinkDeliversValueOnStdinNotCommandLine is the security property of the credential
// sink: the secret value is delivered to the provider CLI on standard input and never
// appears on the command line, where another process could read it. The command line names
// only the app and the operation.
func TestFlySinkDeliversValueOnStdinNotCommandLine(t *testing.T) {
	const value = "sup3r-secret-passphrase"
	sb := &recordingSandbox{}
	sink := NewCredentialSink(fixedSource{val: secret.New(value)}, sb)

	err := sink.Put(context.Background(), "fly", "flynn/vault-passphrase", map[string]string{
		"app": "flyion-test",
		"key": "FLYNN_VAULT_PASSPHRASE",
		"cli": "/deps/flyctl",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if strings.Contains(sb.line, value) {
		t.Fatalf("secret value leaked onto the command line: %q", sb.line)
	}
	if !strings.Contains(sb.line, "secrets import") || !strings.Contains(sb.line, "flyion-test") {
		t.Fatalf("unexpected command line: %q", sb.line)
	}
	if got := string(sb.stdin); got != "FLYNN_VAULT_PASSPHRASE="+value+"\n" {
		t.Fatalf("secret not delivered on stdin as KEY=value; got %q", got)
	}
}

// TestFlySinkSurfacesAFailure proves a non-zero provider exit becomes an error naming the
// key and app (both safe identifiers), so a failed materialization stops the playbook.
func TestFlySinkSurfacesAFailure(t *testing.T) {
	sb := &recordingSandbox{res: sandbox.ExecResult{ExitCode: 1, Output: "Error: app not found"}}
	sink := NewCredentialSink(fixedSource{val: secret.New("x")}, sb)

	err := sink.Put(context.Background(), "fly", "ref", map[string]string{
		"app": "missing", "key": "K", "cli": "/deps/flyctl",
	})
	if err == nil {
		t.Fatal("a non-zero provider exit must fail the step")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error should name the app: %v", err)
	}
}

// TestSinkRejectsUnknownSink proves an unrecognized sink name fails closed rather than
// silently dropping the secret.
func TestSinkRejectsUnknownSink(t *testing.T) {
	sink := NewCredentialSink(fixedSource{val: secret.New("x")}, &recordingSandbox{})
	if err := sink.Put(context.Background(), "nope", "ref", nil); err == nil {
		t.Fatal("unknown sink must fail closed")
	}
}
