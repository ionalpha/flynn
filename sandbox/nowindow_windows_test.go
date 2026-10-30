//go:build windows

package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// consoleProbeSource reports whether the console this process is attached to has a visible
// window. A console child started by a parent that has no console gets a console of its
// own, and unless the launch says otherwise that console is a window on the user's screen.
const consoleProbeSource = `package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func main() {
	hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	visible := uintptr(0)
	if hwnd != 0 {
		visible, _, _ = windows.NewLazySystemDLL("user32.dll").NewProc("IsWindowVisible").Call(hwnd)
	}
	fmt.Printf("CONSOLE-WINDOW hwnd=%#x visible=%t\n", hwnd, visible != 0)
}
`

const (
	// consolelessChildEnv marks the re-executed test binary that runs the real assertions.
	consolelessChildEnv = "FLYNN_TEST_CONSOLELESS_CHILD"
	// consoleProbeEnv carries the built probe's path across the re-execution.
	consoleProbeEnv = "FLYNN_TEST_CONSOLE_PROBE"
)

// buildConsoleProbe compiles consoleProbeSource into dir and returns its path, skipping
// where there is no Go toolchain to build with.
func buildConsoleProbe(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the console probe")
	}
	src := filepath.Join(t.TempDir(), "consoleprobe")
	if err := os.MkdirAll(src, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(consoleProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "consoleprobe.exe")
	wd, err := os.Getwd() // the sandbox package dir: build inside this module
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", out, filepath.Join(src, "main.go"))
	cmd.Dir = wd
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build console probe: %v: %s", err, b)
	}
	return out
}

// TestChildrenOfAConsolelessParentOpenNoWindow is the regression test for the defect where
// every command the agent ran opened a console window. Flynn run from a launcher, a
// service, or a GUI host has no console, and each console child it starts then gets one of
// its own unless the launch asks for none. The test re-executes itself with
// DETACHED_PROCESS so the parent really has no console, then starts the probe through every
// way the sandbox starts a child and fails on any that is attached to a visible window.
func TestChildrenOfAConsolelessParentOpenNoWindow(t *testing.T) {
	if os.Getenv(consolelessChildEnv) == "" {
		probe := buildConsoleProbe(t, t.TempDir())
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestChildrenOfAConsolelessParentOpenNoWindow$", "-test.v", "-test.timeout=2m") //nolint:gosec // re-executes this test binary
		cmd.Env = append(os.Environ(), consolelessChildEnv+"=1", consoleProbeEnv+"="+probe)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("a child of a parent with no console opened a window: %v\n%s", err, out)
		}
		return
	}

	built := os.Getenv(consoleProbeEnv)
	if hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call(); hwnd != 0 {
		t.Fatal("the re-executed test still has a console, so the scenario under test is not set up")
	}

	tiers := []struct {
		name string
		opts []LocalOption
		// confined is set for the AppContainer launch, which only Capture and Stream can
		// use here: Serve and Session refuse it on Windows, and a shell line run through
		// Exec is denied inside the container from a parent with no console. The
		// write-restricted tier is left out because it does not start from such a parent at
		// all, with or without a console flag.
		confined bool
	}{
		{"unconfined", nil, false},
		{"appcontainer", []LocalOption{WithReadOnlyFS(), WithSeccomp()}, true},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			root := t.TempDir()
			// The probe lives in the workspace: a confined child can read nothing else.
			probe := filepath.Join(root, "consoleprobe.exe")
			bin, err := os.ReadFile(built)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(probe, bin, 0o700); err != nil { //nolint:gosec // an executable test fixture
				t.Fatal(err)
			}
			opts := append([]LocalOption{WithExecTimeout(30 * time.Second)}, tier.opts...)
			loc, err := NewLocal(root, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = loc.Close() }()
			ctx := context.Background()

			if !tier.confined {
				t.Run("Exec", func(t *testing.T) {
					res, err := loc.Exec(ctx, Command{Line: probe})
					if err != nil {
						t.Fatalf("exec: %v", err)
					}
					requireNoVisibleWindow(t, res.Output)
				})
			}
			t.Run("Capture", func(t *testing.T) {
				res, err := loc.Capture(ctx, CaptureSpec{Argv: []string{probe}})
				if err != nil {
					t.Fatalf("capture: %v", err)
				}
				requireNoVisibleWindow(t, res.Output)
			})
			t.Run("Stream", func(t *testing.T) {
				p, err := loc.Stream(ctx, StreamSpec{Argv: []string{probe}, Confine: tier.confined})
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
				out, _ := io.ReadAll(p.Stdout())
				_ = p.Wait()
				requireNoVisibleWindow(t, string(out))
			})
			if tier.confined {
				return // Serve and Session refuse a confined launch on Windows
			}
			t.Run("Serve", func(t *testing.T) {
				p, err := loc.Serve(ctx, ServeSpec{Argv: []string{probe}})
				if err != nil {
					t.Fatalf("serve: %v", err)
				}
				wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				if err := p.Wait(wctx); err != nil {
					t.Fatalf("wait: %v", err)
				}
				requireNoVisibleWindow(t, p.Output())
			})
			t.Run("Session", func(t *testing.T) {
				s, err := loc.Session(ctx, SessionSpec{Argv: []string{probe}})
				if err != nil {
					t.Fatalf("session: %v", err)
				}
				defer func() { _ = s.Stop() }()
				out, _ := io.ReadAll(s.Stdout())
				requireNoVisibleWindow(t, string(out))
			})
		})
	}
}

// requireNoVisibleWindow fails unless the probe ran and reported no visible console window.
func requireNoVisibleWindow(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "CONSOLE-WINDOW") {
		t.Fatalf("the probe did not report: %q", out)
	}
	if !strings.Contains(out, "visible=false") {
		t.Fatalf("the child is attached to a visible console window: %s", strings.TrimSpace(out))
	}
}
