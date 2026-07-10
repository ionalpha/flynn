// Package e2e drives the real, compiled flynn binary as a subprocess and asserts on
// observable behavior: exit code, files produced or withheld, stderr refusal text, and
// the emitted verifiable record. It is a black box: nothing here imports flynn's
// internal packages, so it exercises exactly what a user (or an attacker) reaches
// through the shipped artifact and its defaults.
//
// Determinism and offline operation come from a scripted, in-process OpenAI-compatible
// server (fakeopenai_test.go): the binary is pointed at it with OPENAI_BASE_URL, so no
// run needs a live API key or the network. Exactly one lane (the real-model smoke,
// guarded by FLYNN_E2E_REAL) hits a hosted model, and it runs benign scenarios only.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stampedVersion is the version the suite links into its own build of the binary, so a
// test can assert the --version round-trip: the release stamps this same variable via
// -ldflags, and a build that fails to stamp would report the source default instead.
const stampedVersion = "v0.0.0-e2e"

// flynnBin is the path to the binary built once in TestMain and shared by every test.
var flynnBin string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

// runMain builds the binary and runs the suite, returning the process exit code. It is
// split from TestMain so a build failure can be reported without leaking a temp dir.
func runMain(m *testing.M) int {
	bin, cleanup, err := buildFlynn()
	if err != nil {
		// A build failure is a suite failure, not a skip: the shipped artifact must
		// compile on every target.
		println("e2e: building flynn:", err.Error())
		return 1
	}
	defer cleanup()
	flynnBin = bin
	return m.Run()
}

// buildFlynn compiles ./cmd/flynn once with the version stamp and returns the binary
// path and a cleanup. The build carries the same -ldflags -X the release uses, so the
// version-stamp assertion exercises the real stamping path.
func buildFlynn() (string, func(), error) {
	dir, err := os.MkdirTemp("", "flynn-e2e-bin")
	if err != nil {
		return "", nil, err
	}
	bin := filepath.Join(dir, "flynn")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ldflags := "-X github.com/ionalpha/flynn/internal/version.Version=" + stampedVersion
	cmd := exec.CommandContext(ctx, "go", "build", "-ldflags", ldflags, "-o", bin, "../cmd/flynn")
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, errors.New(err.Error() + ": " + string(out))
	}
	return bin, func() { _ = os.RemoveAll(dir) }, nil
}

// instance is one isolated flynn install: its own durable data dir, its own workspace
// (the process cwd, where a goal's file artifacts land), and a scrubbed environment.
// Every run through it is hermetic, so tests neither touch the developer's real flynn
// state nor each other's.
type instance struct {
	t         *testing.T
	dataDir   string
	workspace string
	env       []string
	model     string
}

// newInstance builds an isolated instance with no model server attached. Attach a
// scripted server with withModel before running a goal.
func newInstance(t *testing.T) *instance {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "home")
	for _, d := range []string{data, work, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &instance{
		t:         t,
		dataDir:   data,
		workspace: work,
		env:       scrubbedEnv(home),
		model:     "openai:gpt-e2e",
	}
}

// withModel points the instance at a scripted server: OPENAI_BASE_URL reaches it and a
// dummy key satisfies the adapter. Returns the instance for chaining.
func (in *instance) withModel(f *fakeOpenAI) *instance {
	in.setEnv("OPENAI_API_KEY", "sk-e2e-not-a-real-key")
	in.setEnv("OPENAI_BASE_URL", f.baseURL())
	return in
}

// setEnv sets or replaces an environment variable for subsequent runs.
func (in *instance) setEnv(key, val string) {
	prefix := key + "="
	for i, e := range in.env {
		if strings.HasPrefix(e, prefix) {
			in.env[i] = prefix + val
			return
		}
	}
	in.env = append(in.env, prefix+val)
}

// result is the observable outcome of one subprocess run.
type result struct {
	stdout string
	stderr string
	code   int
	err    error // non-nil only on a failure to start/wait (not a non-zero exit)
}

// combined returns stdout and stderr joined, for substring assertions that do not care
// which stream carried the text.
func (r result) combined() string { return r.stdout + "\n" + r.stderr }

// run executes the binary with the instance's data dir, workspace, and environment,
// prepending the shared -data-dir and -model flags. It never fails the test on a
// non-zero exit (a refusal is a valid outcome to assert on); it fails only if the
// process could not be run at all.
func (in *instance) run(args ...string) result {
	return in.runInput(nil, args...)
}

// baseFlags are the shared flags every invocation carries: the data dir, and the model
// when one is set. An empty model omits the -model flag, so a test can exercise the
// launch-time default resolution (an explicit flag versus the persisted default).
func (in *instance) baseFlags() []string {
	flags := []string{"-data-dir", in.dataDir}
	if in.model != "" {
		flags = append(flags, "-model", in.model)
	}
	return flags
}

// runInput is run with stdin wired to input (nil for none). A goal reads no stdin in
// the non-interactive path, but auth and consent flows can.
func (in *instance) runInput(stdin []byte, args ...string) result {
	in.t.Helper()
	full := append(in.baseFlags(), args...)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, flynnBin, full...)
	cmd.Dir = in.workspace
	cmd.Env = in.env
	// If the process outlives its deadline, dump its goroutine stacks before killing it,
	// so a hang is diagnosable from the captured stderr rather than an opaque timeout.
	// SIGQUIT triggers the Go runtime's stack dump (GOTRACEBACK=all is set in the env);
	// WaitDelay bounds the wait for it to flush and exit. Windows has no SIGQUIT, and its
	// leg does not hang, so it keeps the default context kill.
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGQUIT) }
		cmd.WaitDelay = 8 * time.Second
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	runErr := cmd.Run()
	res := result{stdout: out.String(), stderr: errb.String()}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.code = 0
	case errors.As(runErr, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		res.err = runErr
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.err = context.DeadlineExceeded
	}
	if res.err != nil {
		in.t.Fatalf("running flynn %v: %v\nstdout:\n%s\nstderr:\n%s", args, res.err, res.stdout, res.stderr)
	}
	in.dumpOnFailure(args, res)
	return res
}

// running is a flynn subprocess started in the background, for scenarios that kill it
// mid-run. Its output is captured; kill terminates it and waits for exit.
type running struct {
	cmd  *exec.Cmd
	out  *bytes.Buffer
	errb *bytes.Buffer
	done chan error
}

// start launches the binary in the background with the shared flags and returns a handle
// the test can kill. Unlike run it does not wait for exit, so the caller can terminate
// the process at a chosen point (a crash/resume scenario).
func (in *instance) start(args ...string) *running {
	in.t.Helper()
	full := append(in.baseFlags(), args...)
	cmd := exec.Command(flynnBin, full...)
	cmd.Dir = in.workspace
	cmd.Env = in.env
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		in.t.Fatalf("starting flynn %v: %v", args, err)
	}
	r := &running{cmd: cmd, out: &out, errb: &errb, done: make(chan error, 1)}
	go func() { r.done <- cmd.Wait() }()
	in.t.Cleanup(func() {
		// A test that returns without killing must not leak the process.
		_ = r.cmd.Process.Kill()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
	})
	return r
}

// kill hard-terminates the process (TerminateProcess on Windows, SIGKILL elsewhere) and
// waits for it to exit, mirroring an abrupt crash mid-run.
func (r *running) kill(t *testing.T) {
	t.Helper()
	_ = r.cmd.Process.Kill()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit after kill")
	}
}

// runIDRe matches the run id flynn prints when a goal starts ("  run <uuid>").
var runIDRe = regexp.MustCompile(`run ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// runID extracts the run id a goal printed, failing the test if none was found.
func (in *instance) runID(res result) string {
	in.t.Helper()
	m := runIDRe.FindStringSubmatch(res.stdout)
	if m == nil {
		in.t.Fatalf("no run id in goal output:\n%s", res.stdout)
	}
	return m[1]
}

// verify runs `spine verify <runID>` and returns its result, for the caller to assert
// exit code and per-tier report text.
func (in *instance) verify(runID string) result {
	return in.run("spine", "verify", runID)
}

// export runs `spine export --out <path> <runID>` and returns the record file path.
func (in *instance) export(runID string) (string, result) {
	path := filepath.Join(in.t.TempDir(), runID+".flynnrecord")
	res := in.run("spine", "export", "--out", path, runID)
	return path, res
}

// workfile reads a file the goal was meant to produce in the workspace.
func (in *instance) workfile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(in.workspace, name))
}

// dumpOnFailure registers a cleanup that, if the test failed, writes the run's stdout,
// stderr, and a copy of the data dir to the artifacts directory (FLYNN_E2E_ARTIFACTS,
// defaulting under the test temp), so a CI failure is triageable from the captured
// record and logs rather than only a red check.
func (in *instance) dumpOnFailure(args []string, res result) {
	in.t.Cleanup(func() {
		if !in.t.Failed() {
			return
		}
		dir := os.Getenv("FLYNN_E2E_ARTIFACTS")
		if dir == "" {
			return // no artifact sink configured; the failure message already carries the logs
		}
		base := filepath.Join(dir, sanitize(in.t.Name()))
		_ = os.MkdirAll(base, 0o755)
		_ = os.WriteFile(filepath.Join(base, "cmd.txt"), []byte(strings.Join(args, " ")), 0o644)
		_ = os.WriteFile(filepath.Join(base, "stdout.txt"), []byte(res.stdout), 0o644)
		_ = os.WriteFile(filepath.Join(base, "stderr.txt"), []byte(res.stderr), 0o644)
		copyTree(in.dataDir, filepath.Join(base, "data"))
	})
}

// scrubbedEnv returns the process environment with every provider credential and
// base-URL override removed and HOME/USERPROFILE/APPDATA/LOCALAPPDATA repointed at an
// isolated home, so an inherited key or config on the developer's machine can neither
// leak into a run nor make it non-deterministic. Everything else (PATH, SYSTEMROOT,
// TEMP) is kept, which the binary and its sandboxed child processes need.
//
// The GitHub variables are dropped for the same reason and one more: this suite runs
// inside GitHub Actions, which exports GITHUB_TOKEN and GITHUB_API_URL into every job.
// Left in place they would point a review at the live API with a live credential, so a
// test that meant to talk to a scripted server would talk to github.com instead.
func scrubbedEnv(home string) []string {
	drop := map[string]bool{
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_BASE_URL": true,
		"OPENAI_API_KEY": true, "OPENAI_BASE_URL": true,
		"DEEPSEEK_API_KEY": true, "DEEPSEEK_BASE_URL": true,
		"GEMINI_API_KEY": true, "GEMINI_BASE_URL": true,
		"LLAMACPP_BASE_URL": true, "LLAMACPP_VISION": true,
		"GITHUB_TOKEN": true, "GITHUB_API_URL": true,
		"FLYNN_GITHUB_APP_ISSUER": true, "FLYNN_GITHUB_APP_INSTALLATION": true,
		"FLYNN_GITHUB_APP_KEY": true, "FLYNN_GITHUB_APP_KEY_FILE": true,
	}
	var out []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		up := strings.ToUpper(k)
		if drop[up] {
			continue
		}
		switch up {
		case "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_DATA_HOME", "XDG_CONFIG_HOME":
			continue
		case "FLYNN_VAULT_PASSPHRASE", "FLYNN_VAULT_FILE", "GOTRACEBACK":
			continue // set to fixed values below, not inherited
		}
		out = append(out, e)
	}
	out = append(
		out,
		"HOME="+home,
		"USERPROFILE="+home,
		"APPDATA="+filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		// The binary needs its instance signing identity to seal a run's verifiable
		// record, and creating that identity writes it to the vault. FLYNN_VAULT_FILE
		// pins the sealed-file backend so the vault never touches the host OS keychain:
		// on a macOS runner the login keychain is locked and go-keyring's /usr/bin/security
		// calls block on it, which would hang every command; on every OS this keeps the
		// suite hermetic and off the developer's real keychain. The fixed passphrase below
		// unlocks that file so the identity persists and every run seals on every OS.
		"FLYNN_VAULT_FILE=1",
		"FLYNN_VAULT_PASSPHRASE=flynn-e2e-fixed-passphrase",
		// Ask the Go runtime for every goroutine's stack on an abnormal signal, so the
		// deadline dump below shows exactly where a hung binary was blocked.
		"GOTRACEBACK=all",
	)
	return out
}

// sanitize turns a test name into a filesystem-safe directory name.
func sanitize(name string) string {
	return strings.NewReplacer("/", "_", "\\", "_", " ", "_").Replace(name)
}

// copyTree copies a directory tree best-effort for artifact capture; errors are ignored
// because a triage copy must never fail a test.
func copyTree(src, dst string) {
	_ = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // artifact capture is best-effort; a walk error just skips that entry
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			_ = os.MkdirAll(target, 0o755)
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // p comes from walking the test's own temp data dir
		if err != nil {
			return nil //nolint:nilerr // best-effort: skip a file we cannot read rather than fail the test
		}
		_ = os.WriteFile(target, b, 0o600)
		return nil
	})
}

// requireContains fails the test unless got contains want, printing the full text so a
// mismatch is diagnosable.
func requireContains(t *testing.T, got, want, ctx string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%s: expected to contain %q, got:\n%s", ctx, want, got)
	}
}

// requireExit fails the test unless the run exited with want.
func requireExit(t *testing.T, res result, want int, ctx string) {
	t.Helper()
	if res.code != want {
		t.Fatalf("%s: expected exit %d, got %d\nstdout:\n%s\nstderr:\n%s", ctx, want, res.code, res.stdout, res.stderr)
	}
}

// scanLines returns the non-empty lines of s, trimmed, for line-oriented assertions
// (for example the run table).
func scanLines(s string) []string {
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
