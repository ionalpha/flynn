package externagent

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/sandbox"
)

// TestSpawnerHelperProcess is the child end of the spawner tests: re-executed as a plain
// subprocess (never a real test), it emits a line and exits with a requested code, driven
// by the environment the sandbox granted it. Running the test binary directly keeps the
// tests portable and exercises the deny-by-default environment for real.
func TestSpawnerHelperProcess(_ *testing.T) {
	if os.Getenv("SPAWNER_HELPER") != "1" {
		return
	}
	_, _ = os.Stdout.WriteString("episode-line\n")
	if code, _ := strconv.Atoi(os.Getenv("SPAWNER_EXIT")); code != 0 {
		os.Exit(code)
	}
	os.Exit(0)
}

// helperInvocation runs the test binary back into TestSpawnerHelperProcess as a plain
// subprocess, with the given exit code and any extra env.
func helperInvocation(exit int) Invocation {
	env := []string{"SPAWNER_HELPER=1"}
	if exit != 0 {
		env = append(env, "SPAWNER_EXIT="+strconv.Itoa(exit))
	}
	return Invocation{
		Path: os.Args[0],
		Args: []string{"-test.run=TestSpawnerHelperProcess"},
		Env:  env,
	}
}

func TestEpisodePolicyDenyAllWithoutHosts(t *testing.T) {
	p := episodePolicy(nil)
	if p.AllowPublic {
		t.Fatal("an empty allowlist must not permit public egress")
	}
	if len(p.AllowHosts) != 0 {
		t.Fatalf("AllowHosts = %v, want empty", p.AllowHosts)
	}
	// A default-deny policy denies every name's address gate, so no provider is reachable.
	if p.AllowsHost("api.example.com") && p.AllowPublic {
		t.Fatal("deny-all must not admit a provider")
	}
}

func TestEpisodePolicyGatesToProviders(t *testing.T) {
	p := episodePolicy([]string{".openai.com", "auth.example.com"})
	if !p.AllowPublic {
		t.Fatal("a provider allowlist must pass the public address gate")
	}
	if !p.AllowsHost("api.openai.com") {
		t.Error("a subdomain of an allowed provider should pass the name gate")
	}
	if !p.AllowsHost("auth.example.com") {
		t.Error("an exact allowed host should pass the name gate")
	}
	if p.AllowsHost("evil.test") {
		t.Error("a name off the allowlist must be denied")
	}
}

func TestNewSandboxSpawnerDefaultsToKernelFloor(t *testing.T) {
	s := NewSandboxSpawner(SandboxConfig{})
	if s.cfg.MinContainment != sandbox.ContainmentKernel {
		t.Fatalf("default MinContainment = %s, want kernel", s.cfg.MinContainment)
	}
	// An explicit floor is preserved.
	s = NewSandboxSpawner(SandboxConfig{MinContainment: sandbox.ContainmentRemote})
	if s.cfg.MinContainment != sandbox.ContainmentRemote {
		t.Fatalf("explicit MinContainment = %s, want remote", s.cfg.MinContainment)
	}
}

func TestStartRefusesBelowContainmentFloor(t *testing.T) {
	// A local sandbox can never reach the remote tier, so a remote floor forces the
	// refuse-rather-than-downgrade path regardless of host.
	s := NewSandboxSpawner(SandboxConfig{MinContainment: sandbox.ContainmentRemote})
	proc, err := s.Start(context.Background(), Episode{Workdir: t.TempDir()}, helperInvocation(0))
	if err == nil {
		if proc != nil {
			_ = proc.Wait()
		}
		t.Fatal("Start should refuse when the host cannot meet the containment floor")
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("error = %v, want a refuse-to-start message", err)
	}
	if proc != nil {
		t.Fatal("a refused start must not return a process")
	}
}

func TestProbeReturnsOutput(t *testing.T) {
	s := NewSandboxSpawner(SandboxConfig{})
	name, args := probeArgv(0)
	out, err := s.Probe(context.Background(), name, args...)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !strings.Contains(out, "probe-ok") {
		t.Fatalf("probe output = %q, want it to contain probe-ok", out)
	}
}

func TestProbeNonZeroExitIsError(t *testing.T) {
	s := NewSandboxSpawner(SandboxConfig{})
	name, args := probeArgv(3)
	_, err := s.Probe(context.Background(), name, args...)
	if err == nil {
		t.Fatal("Probe should return an error for a non-zero exit")
	}
}

// TestStartRunsConfinedEpisode drives a real confined episode through Start, so the
// streaming lifecycle and per-episode teardown are exercised end to end on a host that
// can provide the containment. Egress is left unconfigured (deny-all), so the child needs
// no network; the point is the launch and teardown, not the provider path. It skips where
// the host cannot enforce the boundary (kernel confinement or governed egress absent, e.g.
// a CI runner with unprivileged user namespaces restricted, or native Windows): the
// refusal there is the correct refuse-rather-than-downgrade behavior, not a test failure.
func TestStartRunsConfinedEpisode(t *testing.T) {
	s := NewSandboxSpawner(SandboxConfig{})
	proc, err := s.Start(context.Background(), Episode{Workdir: t.TempDir()}, helperInvocation(0))
	if err != nil {
		if hostCannotConfine(err) {
			t.Skipf("host cannot enforce the confinement boundary: %v", err)
		}
		t.Fatalf("Start: %v", err)
	}
	sc := bufio.NewScanner(proc.Stdout())
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if strings.Join(lines, ",") != "episode-line" {
		t.Fatalf("lines = %v, want [episode-line]", lines)
	}
}

// hostCannotConfine reports whether a Start error is the host refusing to provide the
// confinement boundary rather than a real launch failure: either the containment floor is
// unmet (kernel confinement not enforceable here) or governed egress has no enforcement
// leg on this platform. Both are the correct refuse-rather-than-downgrade behavior, so a
// live-episode test skips on them instead of failing.
func hostCannotConfine(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "refusing to start") ||
		strings.Contains(msg, "governed egress is not enforceable")
}

// probeArgv is a portable argv that prints "probe-ok" and exits with the given code,
// run directly (no shell) so Capture can exec it.
func probeArgv(exit int) (string, []string) {
	if runtime.GOOS == "windows" {
		if exit != 0 {
			return "cmd", []string{"/c", "echo probe-ok & exit " + strconv.Itoa(exit)}
		}
		return "cmd", []string{"/c", "echo probe-ok"}
	}
	if exit != 0 {
		return "sh", []string{"-c", "echo probe-ok; exit " + strconv.Itoa(exit)}
	}
	return "sh", []string{"-c", "echo probe-ok"}
}

// Ensure the probe timeout option threads through without changing behavior on a fast
// command.
func TestProbeHonorsTimeoutOption(t *testing.T) {
	s := NewSandboxSpawner(SandboxConfig{ProbeTimeout: 5 * time.Second})
	name, args := probeArgv(0)
	out, err := s.Probe(context.Background(), name, args...)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !strings.Contains(out, "probe-ok") {
		t.Fatalf("probe output = %q", out)
	}
}
