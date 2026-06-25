package sandbox

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// dumpEnvCommand returns a shell command that prints the whole environment, so a
// test can assert what a sandboxed command can and cannot see.
func dumpEnvCommand() string {
	if runtime.GOOS == "windows" {
		return "set"
	}
	return "env"
}

// TestExecDoesNotLeakHostSecret is the load-bearing security test: a secret placed
// in the agent's own process environment must never appear in a command the
// sandbox runs. This is the execution-boundary half of the "no spawned process env
// contains a raw secret" invariant.
func TestExecDoesNotLeakHostSecret(t *testing.T) {
	const (
		secretKey = "FLYNN_TEST_API_KEY"
		secretVal = "sk-do-not-leak-9c3f2a"
	)
	t.Setenv(secretKey, secretVal)

	sb, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), Command{Line: dumpEnvCommand()})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.Contains(res.Output, secretVal) {
		t.Fatalf("sandboxed command saw the host secret value:\n%s", res.Output)
	}
	if strings.Contains(res.Output, secretKey) {
		t.Fatalf("sandboxed command saw the host secret key name:\n%s", res.Output)
	}
}

// TestExecEnvIsBaselineOnly asserts the positive side of the contract: the command
// still gets the baseline it needs to run (a PATH), so isolation does not break
// ordinary commands.
func TestExecEnvIsBaselineOnly(t *testing.T) {
	sb, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := sb.env()
	var hasPath bool
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
		}
		key, _, _ := strings.Cut(e, "=")
		if !baselineKeyAllowed(key) {
			t.Fatalf("env contained a non-baseline key %q: %v", key, env)
		}
	}
	if !hasPath {
		t.Fatalf("env lacked PATH, commands would not find tools: %v", env)
	}
}

// TestWithEnvGrantsExplicitVar checks the brokered path: a variable granted via
// WithEnv reaches the command, and only that one beyond the baseline.
func TestWithEnvGrantsExplicitVar(t *testing.T) {
	sb, err := NewLocal(t.TempDir(), WithEnv(map[string]string{"FLYNN_GRANTED": "yes-123"}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := sb.Exec(context.Background(), Command{Line: dumpEnvCommand()})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Output, "yes-123") {
		t.Fatalf("granted variable did not reach the command:\n%s", res.Output)
	}
}

func baselineKeyAllowed(key string) bool {
	for _, k := range baselineEnvKeys {
		if k == key {
			return true
		}
	}
	return false
}
