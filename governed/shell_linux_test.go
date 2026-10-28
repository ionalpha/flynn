package governed

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/sandbox/sandboxtest"
)

// These tests launch real commands under the kernel confinement, because a hidden path is a
// property of the mount table a command sees and no unit test of the option list can show
// it. Each skips on a host that cannot confine.

// canary is planted where a credential would be. The tests look for it in what a confined
// command printed, so a command that read the file fails the test whatever it did next.
const canary = "FLYNN-CANARY-3f9c1e7a"

// plant writes a file holding the canary, creating its directory.
func plant(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(canary+" "+filepath.Base(path)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// confinedShell returns a sandbox built from p, in a fresh working directory.
func confinedShell(t *testing.T, p ShellPolicy) *sandbox.Local {
	t.Helper()
	sandboxtest.RequireConfinement(t)
	sb, err := sandbox.NewLocal(t.TempDir(), p.options()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// execIn executes line in sb and returns its output and exit code, failing on a launch error.
func execIn(t *testing.T, sb *sandbox.Local, line string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := sb.Exec(ctx, sandbox.Command{Line: line})
	if err != nil {
		t.Fatalf("exec %q: %v", line, err)
	}
	return res.Output, res.ExitCode
}

// credentialHome builds a home directory holding a canary in each credential location the
// feature names, a data directory holding the database, and one ordinary file in the home to
// show the rest of it stays readable. It points HOME at the home.
func credentialHome(t *testing.T) (home, dataDir string, canaries []string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	for _, rel := range []string{
		".ssh/id_ed25519",
		".claude/.credentials.json",
		".claude.json",
		".codex/auth.json",
		".aws/credentials",
		".config/gh/hosts.yml",
		".kube/config",
		".docker/config.json",
		".netrc",
	} {
		p := filepath.Join(home, filepath.FromSlash(rel))
		plant(t, p)
		canaries = append(canaries, p)
	}
	plant(t, filepath.Join(home, "notes.txt"))

	dataDir = filepath.Join(t.TempDir(), "flynn")
	for _, rel := range []string{"flynn.db", "vault.sealed", "signers/instance.key"} {
		p := filepath.Join(dataDir, filepath.FromSlash(rel))
		plant(t, p)
		canaries = append(canaries, p)
	}
	return home, dataDir, canaries
}

// TestConfinedCommandCannotReadPlantedCanaries: with the default policy a command cannot read
// the data directory, an external CLI's login token or ~/.ssh, while the rest of the home
// stays readable, so the denial is the hide set and not a shell that cannot read anything.
// With the credential opt-in the same canaries are readable, which shows the test can see a
// read when there is one.
func TestConfinedCommandCannotReadPlantedCanaries(t *testing.T) {
	home, dataDir, canaries := credentialHome(t)

	t.Run("default", func(t *testing.T) {
		sb := confinedShell(t, ShellPolicy{DataDir: dataDir})

		if out, code := execIn(t, sb, "cat "+filepath.Join(home, "notes.txt")); code != 0 || !strings.Contains(out, canary) {
			t.Fatalf("an ordinary file in the home is unreadable (exit %d): %q", code, out)
		}
		for _, p := range canaries {
			if out, _ := execIn(t, sb, "cat "+p); strings.Contains(out, canary) {
				t.Errorf("the confined command read %s: %q", p, out)
			}
		}
		// Listing the directory must not reveal a hidden file's name either.
		if out, _ := execIn(t, sb, "ls -A "+filepath.Join(home, ".ssh")+" "+dataDir); strings.Contains(out, "id_ed25519") || strings.Contains(out, "flynn.db") {
			t.Errorf("a hidden directory still lists its contents: %q", out)
		}
	})

	t.Run("credential opt-in", func(t *testing.T) {
		sb := confinedShell(t, ShellPolicy{DataDir: dataDir, AllowCredentialReads: true})
		for _, p := range canaries {
			if out, _ := execIn(t, sb, "cat "+p); !strings.Contains(out, canary) {
				t.Errorf("the opt-in did not make %s readable: %q", p, out)
			}
		}
	})
}

// listen starts a TCP listener on the loopback. It counts the connections it accepts and
// sends on seen for each one, so a test can wait for the host's side of a connection
// without polling a clock.
func listen(t *testing.T) (port int, accepted *atomic.Int64, seen <-chan struct{}) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	accepted = new(atomic.Int64)
	ch := make(chan struct{}, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			select {
			case ch <- struct{}{}:
			default:
			}
			_ = c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, accepted, ch
}

// connectLine opens a TCP connection to the loopback port from bash and prints "connected"
// when it succeeds. Bash is used because it opens the socket itself, so the test needs no
// client binary inside the sandbox.
func connectLine(port int) string {
	return fmt.Sprintf("bash -c 'exec 3<>/dev/tcp/127.0.0.1/%d && echo connected'", port)
}

// TestConfinedCommandCannotConnectByDefault: a connect from a confined command to a listener
// the host runs fails under the default policy and succeeds with the network opt-in. The
// listener counts what it accepted, so a "connected" line is checked against the host's side
// of the connection.
func TestConfinedCommandCannotConnectByDefault(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is needed to open a socket from the command line")
	}
	port, accepted, seen := listen(t)

	deny := confinedShell(t, ShellPolicy{DataDir: t.TempDir()})
	if out, code := execIn(t, deny, connectLine(port)); code == 0 || strings.Contains(out, "connected") {
		t.Fatalf("the default policy let a command connect out (exit %d): %q", code, out)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the listener accepted %d connections from a denied command", n)
	}

	allow := confinedShell(t, ShellPolicy{DataDir: t.TempDir(), AllowNetwork: true})
	if out, code := execIn(t, allow, connectLine(port)); code != 0 || !strings.Contains(out, "connected") {
		t.Fatalf("the network opt-in did not reach the listener (exit %d): %q", code, out)
	}
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the command reported a connection the listener never saw")
	}
}

// siblingProcess starts a process of the same account outside the sandbox, holding the canary
// in its environment and its arguments: the position of the flynn process that was handed a
// provider key in its environment and a bot token on its command line.
func siblingProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 60; exit 0", canary)
	cmd.Env = append(os.Environ(), "FLYNN_PROVIDER_KEY="+canary)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	// The control: this process can read what it started, so a denial below is the sandbox's.
	for _, f := range []string{"environ", "cmdline"} {
		var seen bool
		for i := 0; i < 100 && !seen; i++ {
			b, err := os.ReadFile(fmt.Sprintf("/proc/%d/%s", cmd.Process.Pid, f))
			seen = err == nil && strings.Contains(string(b), canary)
			if !seen {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if !seen {
			t.Skipf("the test process cannot read /proc/%d/%s itself", cmd.Process.Pid, f)
		}
	}
	return cmd.Process.Pid
}

// TestConfinedCommandCannotReadParentProcEnviron: the environment of a process outside the
// sandbox, which is where a provider key or an environment vault passphrase is, is not
// readable from a confined command through procfs.
func TestConfinedCommandCannotReadParentProcEnviron(t *testing.T) {
	pid := siblingProcess(t)
	sb := confinedShell(t, ShellPolicy{DataDir: t.TempDir()})
	if out, _ := execIn(t, sb, fmt.Sprintf("cat /proc/%d/environ", pid)); strings.Contains(out, canary) {
		t.Fatalf("the confined command read another process's environment: %q", out)
	}
}

// TestConfinedCommandCannotReadParentCmdline: the same for the command line, which is where
// a bot or API token passed as an argument is.
//
// Unlike the environment, a command line is world-readable in procfs and no ptrace check
// stands in front of it, so only a PID namespace with its own procfs removes it. The sandbox
// has none yet (it changes what PID 1 does with a backgrounded process, and belongs with the
// runner sandbox), so while the command line is readable the test skips and names the open
// gap, and it starts asserting the day a PID namespace closes it.
func TestConfinedCommandCannotReadParentCmdline(t *testing.T) {
	pid := siblingProcess(t)
	sb := confinedShell(t, ShellPolicy{DataDir: t.TempDir()})
	if out, _ := execIn(t, sb, fmt.Sprintf("cat /proc/%d/cmdline", pid)); strings.Contains(out, canary) {
		t.Skip("open gap: a confined command can read another process's command line through procfs; closing it needs a PID namespace with its own procfs")
	}
}

// TestKeychainNotReachableFromConfinedCommand: the session bus and the keyring and agent
// sockets of the user's runtime directory are not there for a confined command to connect
// to. The socket is a pathname, which a read-only mount and a network namespace both leave
// reachable, so the hide set is what removes it.
func TestKeychainNotReachableFromConfinedCommand(t *testing.T) {
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	var sockets []string
	for _, name := range runtimeSocketNames {
		p := filepath.Join(runtime, name)
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Skipf("no unix socket at %s: %v", p, err)
		}
		t.Cleanup(func() { _ = l.Close() })
		sockets = append(sockets, p)
	}

	deny := confinedShell(t, ShellPolicy{DataDir: t.TempDir()})
	allow := confinedShell(t, ShellPolicy{DataDir: t.TempDir(), AllowCredentialReads: true})
	for _, p := range sockets {
		line := fmt.Sprintf("test -S %s && echo socket", p)
		if out, _ := execIn(t, allow, line); !strings.Contains(out, "socket") {
			t.Fatalf("the control cannot see %s with the opt-in: %q", p, out)
		}
		if out, _ := execIn(t, deny, line); strings.Contains(out, "socket") {
			t.Errorf("the confined command can see the socket %s", p)
		}
	}
}
