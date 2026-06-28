package playbook

import (
	"context"
	"errors"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/secret"
)

// TestPropFlySinkNeverLeaksValueOnCommandLine is the credential sink's headline invariant,
// fuzzed: over arbitrary secret values, app names, and key names, the command line is a
// fixed function of the non-secret inputs alone, and the value rides only on standard
// input. No secret value can ever reach the command line, where another process could read
// it from the process table.
func TestPropFlySinkNeverLeaksValueOnCommandLine(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		value := rapid.String().Draw(rt, "value")
		app := rapid.StringMatching(`[a-z][a-z0-9-]{0,20}`).Draw(rt, "app")
		key := rapid.StringMatching(`[A-Z][A-Z0-9_]{0,20}`).Draw(rt, "key")

		sb := &recordingSandbox{}
		sink := NewCredentialSink(fixedSource{val: secret.New(value)}, sb)
		err := sink.Put(context.Background(), "fly", "ref", map[string]string{
			"app": app, "key": key, "cli": "/deps/flyctl",
		})
		if err != nil {
			rt.Fatalf("put: %v", err)
		}
		// The command line is built from the non-secret inputs only; it does not vary with
		// the value, so the value is provably absent from it.
		want := "/deps/flyctl secrets import --stage --app " + app
		if sb.line != want {
			rt.Fatalf("command line is not a fixed function of the non-secret inputs: %q", sb.line)
		}
		// The value is delivered verbatim on standard input.
		if got := string(sb.stdin); got != key+"="+value+"\n" {
			rt.Fatalf("value not delivered verbatim on stdin: %q", got)
		}
	})
}

// FuzzFlySinkNeverLeaksValue drives the sink with arbitrary raw secret bytes and asserts
// the value never appears on the command line, complementing the structured property test
// with unstructured input (control bytes, quotes, the command keywords themselves).
func FuzzFlySinkNeverLeaksValue(f *testing.F) {
	f.Add("plain")
	f.Add("with spaces and 'quotes' and --flags")
	f.Add("secrets import --app evil") // the value mimics the command itself
	f.Add("")
	f.Fuzz(func(t *testing.T, value string) {
		sb := &recordingSandbox{}
		sink := NewCredentialSink(fixedSource{val: secret.New(value)}, sb)
		err := sink.Put(context.Background(), "fly", "ref", map[string]string{
			"app": "app", "key": "K", "cli": "/deps/flyctl",
		})
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		// The command line is exactly the fixed command, whatever the value is, so the
		// value cannot have influenced (and so cannot leak through) the command line. A
		// substring check would false-positive when a value happens to match a literal
		// word in the command; equality to the value-independent command is the true
		// invariant.
		const want = "/deps/flyctl secrets import --stage --app app"
		if sb.line != want {
			t.Fatalf("command line varied with the secret value: %q", sb.line)
		}
		if got := string(sb.stdin); got != "K="+value+"\n" {
			t.Fatalf("value not delivered on stdin: %q", got)
		}
	})
}

// TestFlySinkFailsClosedWhenSourceFaults is the chaos case: when the secret source fails
// (a locked keychain, an unreachable vault), the materialization fails closed and the
// provider CLI is never run, so a resolve failure can never leave a half-applied or empty
// credential on the target.
func TestFlySinkFailsClosedWhenSourceFaults(t *testing.T) {
	sb := &recordingSandbox{}
	src := testkit.FaultySource(fixedSource{val: secret.New("never-read")}, testkit.Always(errors.New("vault unreachable")))
	sink := NewCredentialSink(src, sb)

	err := sink.Put(context.Background(), "fly", "ref", map[string]string{
		"app": "app", "key": "K", "cli": "/deps/flyctl",
	})
	if err == nil {
		t.Fatal("a source failure must fail the materialization closed")
	}
	if sb.line != "" {
		t.Fatalf("the provider CLI must not run when the secret could not be resolved; ran %q", sb.line)
	}
}

// TestFlySinkFailsClosedWhenProviderFaults proves a provider transport failure (the CLI
// could not be run at all) surfaces as an error rather than being swallowed.
func TestFlySinkFailsClosedWhenProviderFaults(t *testing.T) {
	sb := &recordingSandbox{err: errors.New("sandbox unavailable")}
	sink := NewCredentialSink(fixedSource{val: secret.New("x")}, sb)

	if err := sink.Put(context.Background(), "fly", "ref", map[string]string{
		"app": "app", "key": "K", "cli": "/deps/flyctl",
	}); err == nil {
		t.Fatal("a provider transport failure must fail the step")
	}
}
