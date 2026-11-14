package sandbox

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/provider"
)

const envDumpHelper = "FLYNN_SANDBOX_ENVDUMP"

// TestEnvDumpHelperProcess is not a test. The canary tests re-run the test binary with
// [envDumpHelper] set, and this function prints every variable the child received, one
// per line, which is what a tool printing its environment would show.
func TestEnvDumpHelperProcess(_ *testing.T) {
	if os.Getenv(envDumpHelper) != "1" {
		return
	}
	for _, kv := range os.Environ() {
		_, _ = os.Stdout.WriteString(kv + "\n")
	}
	os.Exit(0)
}

func envDumpArgv() []string {
	return []string{os.Args[0], "-test.run=^TestEnvDumpHelperProcess$"}
}

// canaryFor returns a distinctive value for a credential variable, so a leak names its
// source in the failure message.
func canaryFor(name string) string { return "canary-" + strings.ToLower(name) + "-7f3a91" }

// plantCanaries puts a canary in the parent environment under every provider credential
// name and returns the values.
func plantCanaries(t *testing.T) map[string]string {
	t.Helper()
	vals := map[string]string{}
	for _, name := range provider.CredentialEnvVars() {
		vals[name] = canaryFor(name)
		t.Setenv(name, vals[name])
	}
	if len(vals) == 0 {
		t.Fatal("provider.CredentialEnvVars is empty: the canary test would check nothing")
	}
	return vals
}

// assertNoCanary fails when the dump names a credential variable or carries its value.
func assertNoCanary(t *testing.T, path, dump string, canaries map[string]string) {
	t.Helper()
	if !strings.Contains(dump, "PATH=") && !strings.Contains(dump, "Path=") {
		t.Fatalf("%s: the child printed no environment at all, so absence of a canary proves nothing:\n%s", path, dump)
	}
	for name, val := range canaries {
		if strings.Contains(dump, val) {
			t.Errorf("%s: a tool subprocess saw the value of %s", path, name)
		}
		if strings.Contains(dump, name+"=") {
			t.Errorf("%s: a tool subprocess saw the variable %s", path, name)
		}
	}
}

// TestToolSubprocessesNeverSeeProviderCredentials plants a canary under each provider
// credential name in the agent's own environment, then asks a child started by every
// launch path to print what it received. None may show a canary; the sandbox's
// deny-by-default environment is what a headless session's tool subprocesses inherit.
func TestToolSubprocessesNeverSeeProviderCredentials(t *testing.T) {
	canaries := plantCanaries(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	grant := []string{envDumpHelper + "=1"}

	t.Run("exec", func(t *testing.T) {
		l := newTestLocal(t)
		res, err := l.Exec(ctx, Command{Line: dumpEnvCommand()})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		assertNoCanary(t, "Exec", res.Output, canaries)
	})

	t.Run("capture", func(t *testing.T) {
		l := newTestLocal(t)
		res, err := l.Capture(ctx, CaptureSpec{Argv: envDumpArgv(), Env: grant})
		if err != nil {
			t.Fatalf("capture: %v", err)
		}
		assertNoCanary(t, "Capture", res.Output, canaries)
	})

	t.Run("stream", func(t *testing.T) {
		l := newTestLocal(t)
		p, err := l.Stream(ctx, StreamSpec{Argv: envDumpArgv(), Env: grant})
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		out, _ := io.ReadAll(p.Stdout())
		_ = p.Wait()
		assertNoCanary(t, "Stream", string(out), canaries)
	})

	t.Run("session", func(t *testing.T) {
		l := newTestLocal(t)
		s, err := l.Session(ctx, SessionSpec{Argv: envDumpArgv(), Env: grant})
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		defer func() { _ = s.Stop() }()
		out, _ := io.ReadAll(s.Stdout())
		assertNoCanary(t, "Session", string(out), canaries)
	})

	t.Run("serve", func(t *testing.T) {
		l := newTestLocal(t, WithEnv(map[string]string{envDumpHelper: "1"}))
		p, err := l.Serve(ctx, ServeSpec{Argv: envDumpArgv()})
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		defer func() { _ = p.Stop() }()
		if err := p.Wait(ctx); err != nil {
			t.Fatalf("serve wait: %v", err)
		}
		assertNoCanary(t, "Serve", p.Output(), canaries)
	})
}

// TestExplicitGrantOfACredentialStillReaches keeps the canary test honest in the other
// direction: the sandbox does not strip credential names, it withholds what nobody
// granted, so a value granted on purpose reaches the child. Without this a launch path
// that dropped every environment variable would pass the test above.
func TestExplicitGrantOfACredentialStillReaches(t *testing.T) {
	canaries := plantCanaries(t)
	name := provider.CredentialEnvVars()[0]
	granted := "granted-" + canaries[name]

	l := newTestLocal(t)
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: envDumpArgv(),
		Env:  []string{envDumpHelper + "=1", name + "=" + granted},
	})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !strings.Contains(res.Output, name+"="+granted) {
		t.Fatalf("an explicitly granted %s did not reach the child:\n%s", name, res.Output)
	}
	for other, val := range canaries {
		if other != name && strings.Contains(res.Output, val) {
			t.Errorf("granting %s also exposed %s", name, other)
		}
	}
}
