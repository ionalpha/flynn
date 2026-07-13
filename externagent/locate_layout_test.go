package externagent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFile creates a file (and any missing parents) with the given contents, so a test
// can stand up an installation layout on disk.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil { //nolint:gosec // a test fixture standing in for an installed CLI
		t.Fatalf("write %s: %v", path, err)
	}
}

// launcher writes a stand-in for the npm launcher shim: a file that is not a native
// executable on any platform, so resolution must look past it for the real binary. On
// Windows the extension decides (".cmd" is not ".exe"); elsewhere the "#!" line does.
func launcher(t *testing.T, path string) string {
	t.Helper()
	writeFile(t, path, "#!/bin/sh\nexec node \"$@\"\n")
	if isNativeExecutable(path) {
		t.Fatalf("the fixture launcher %s must not read as a native executable", path)
	}
	return path
}

// nativeBinary writes a stand-in for a vendored native binary, named the way the platform
// names one.
func nativeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name+exeSuffix())
	writeFile(t, path, "MZ native binary")
	return path
}

// TestLocateClaudeResolvesPastTheLauncher proves a launcher shim resolves to the native
// binary it stands for, in each of the two installation shapes: the shim sitting in a
// global bin directory beside the package tree, and the shim living inside the package
// itself. Launching the shim would drag its shell or interpreter into the confinement,
// which an unprivileged process cannot always grant read on, so resolution must reach the
// binary. The package root, not the binary's own bin/, is what the confined child is
// granted, so the helper binaries shipped in the same tree stay reachable.
func TestLocateClaudeResolvesPastTheLauncher(t *testing.T) {
	t.Run("shim beside the package tree", func(t *testing.T) {
		root := t.TempDir()
		shim := launcher(t, filepath.Join(root, "claude.cmd"))
		pkg := filepath.Join(root, "node_modules", "@anthropic-ai", "claude-code")
		want := nativeBinary(t, filepath.Join(pkg, "bin"), "claude")

		prog, err := LocateClaude(shim)
		if err != nil {
			t.Fatalf("LocateClaude: %v", err)
		}
		if !sameDir(t, prog.Path, want) {
			t.Errorf("resolved %q, want the vendored native binary %q", prog.Path, want)
		}
		if len(prog.ReadableDirs) != 1 || !sameDir(t, prog.ReadableDirs[0], pkg) {
			t.Errorf("readable dirs = %v, want the package root %q so helper binaries stay reachable", prog.ReadableDirs, pkg)
		}
	})

	t.Run("entry point inside the package", func(t *testing.T) {
		root := t.TempDir()
		pkg := filepath.Join(root, "node_modules", "@anthropic-ai", "claude-code")
		shim := launcher(t, filepath.Join(pkg, "cli.js"))
		want := nativeBinary(t, filepath.Join(pkg, "bin"), "claude")

		prog, err := LocateClaude(shim)
		if err != nil {
			t.Fatalf("LocateClaude: %v", err)
		}
		if !sameDir(t, prog.Path, want) {
			t.Errorf("resolved %q, want %q: the walk up from the launcher did not find the package", prog.Path, want)
		}
	})
}

// TestLocateClaudeLauncherWithoutBinary proves a launcher whose native binary is nowhere to
// be found is a distinct error from a CLI that is not installed. The two need different
// fixes (repair a broken install versus install one at all), so a broken installation must
// not be reported as ErrProgramNotFound and quietly onboarded as missing.
func TestLocateClaudeLauncherWithoutBinary(t *testing.T) {
	shim := launcher(t, filepath.Join(t.TempDir(), "claude.cmd"))
	_, err := LocateClaude(shim)
	if err == nil {
		t.Fatal("a launcher with no native binary beside it must not resolve")
	}
	if errors.Is(err, ErrProgramNotFound) {
		t.Errorf("a broken installation must not report as not-installed: %v", err)
	}
	if !strings.Contains(err.Error(), "launcher script") {
		t.Errorf("error should name the launcher problem, got: %v", err)
	}
}

// TestLocateCodexResolvesPastTheLauncher proves the codex launcher resolves to the
// vendored native binary, in both shapes newer and older releases ship: the binary beside
// the launcher's package, and the binary in the per-platform dependency package under it.
func TestLocateCodexResolvesPastTheLauncher(t *testing.T) {
	triple := rustTargetTriple()
	if triple == "" {
		t.Skipf("no vendored codex binary is published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	vendorRel := filepath.Join("vendor", triple, "codex")

	t.Run("binary beside the package tree", func(t *testing.T) {
		root := t.TempDir()
		shim := launcher(t, filepath.Join(root, "codex.cmd"))
		pkg := filepath.Join(root, "node_modules", "@openai", "codex")
		want := nativeBinary(t, filepath.Join(pkg, vendorRel), "codex")

		prog, err := LocateCodex(shim)
		if err != nil {
			t.Fatalf("LocateCodex: %v", err)
		}
		if !sameDir(t, prog.Path, want) {
			t.Errorf("resolved %q, want the vendored binary %q", prog.Path, want)
		}
		if len(prog.ReadableDirs) != 1 || !sameDir(t, prog.ReadableDirs[0], pkg) {
			t.Errorf("readable dirs = %v, want the package root %q", prog.ReadableDirs, pkg)
		}
	})

	t.Run("binary in the per-platform dependency package", func(t *testing.T) {
		root := t.TempDir()
		pkg := filepath.Join(root, "node_modules", "@openai", "codex")
		shim := launcher(t, filepath.Join(pkg, "bin", "codex.js"))
		platformPkg := filepath.Join(pkg, "node_modules", "@openai", codexPlatformPkg())
		want := nativeBinary(t, filepath.Join(platformPkg, vendorRel), "codex")

		prog, err := LocateCodex(shim)
		if err != nil {
			t.Fatalf("LocateCodex: %v", err)
		}
		if !sameDir(t, prog.Path, want) {
			t.Errorf("resolved %q, want the binary in the per-platform package %q", prog.Path, want)
		}
		if len(prog.ReadableDirs) != 1 || !sameDir(t, prog.ReadableDirs[0], platformPkg) {
			t.Errorf("readable dirs = %v, want the platform package root %q", prog.ReadableDirs, platformPkg)
		}
	})
}

// TestLocateCodexLauncherWithoutBinary proves a codex launcher with no vendored binary is
// reported as a broken installation naming the triple that was looked for, not as a missing
// CLI.
func TestLocateCodexLauncherWithoutBinary(t *testing.T) {
	if rustTargetTriple() == "" {
		t.Skipf("no vendored codex binary is published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	shim := launcher(t, filepath.Join(t.TempDir(), "codex.cmd"))
	_, err := LocateCodex(shim)
	if err == nil {
		t.Fatal("a launcher with no vendored binary must not resolve")
	}
	if errors.Is(err, ErrProgramNotFound) {
		t.Errorf("a broken installation must not report as not-installed: %v", err)
	}
	if !strings.Contains(err.Error(), rustTargetTriple()) {
		t.Errorf("error should name the triple that was looked for, got: %v", err)
	}
}

// TestLocateResolvesANativeBinaryAsIs proves a CLI installed as a plain native executable
// (a package manager that puts a binary straight on PATH) is used as it is, with its own
// directory granted readable, rather than being sent through the launcher search.
func TestLocateResolvesANativeBinaryAsIs(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		locate  func(string) (Program, error)
		binName string
	}{
		{"claude", LocateClaude, "claude"},
		{"codex", LocateCodex, "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := nativeBinary(t, dir, tc.binName)
			if !isNativeExecutable(bin) {
				t.Skipf("the fixture %s does not read as native on this platform", bin)
			}
			prog, err := tc.locate(bin)
			if err != nil {
				t.Fatalf("locate: %v", err)
			}
			if !sameDir(t, prog.Path, bin) {
				t.Errorf("a native binary must be used as is: got %q, want %q", prog.Path, bin)
			}
			if len(prog.ReadableDirs) != 1 || !sameDir(t, prog.ReadableDirs[0], dir) {
				t.Errorf("readable dirs = %v, want the binary's own directory %q", prog.ReadableDirs, dir)
			}
		})
	}
}

// TestLocateMissingCLIReportsNotFound proves an unresolvable name is reported as
// ErrProgramNotFound so detection onboards the install rather than failing the command.
// A bare name that PATH cannot resolve and an absolute path to nothing are both
// not-installed.
func TestLocateMissingCLIReportsNotFound(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "not-here")
	for _, tc := range []struct {
		name   string
		locate func(string) (Program, error)
	}{
		{"claude", LocateClaude},
		{"codex", LocateCodex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.locate("flynn-no-such-cli-on-path"); !errors.Is(err, ErrProgramNotFound) {
				t.Errorf("an unresolvable name must report ErrProgramNotFound, got %v", err)
			}
			if _, err := tc.locate(absent); !errors.Is(err, ErrProgramNotFound) {
				t.Errorf("an absolute path to nothing must report ErrProgramNotFound, got %v", err)
			}
		})
	}
}

// TestIsNativeExecutableRejectsAScript proves the launcher/binary distinction the whole
// resolution rests on: a script never reads as native, and a directory (which cannot be
// exec'd) never does either.
func TestIsNativeExecutableRejectsAScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "shim.cmd")
	writeFile(t, script, "#!/bin/sh\nexec node\n")
	if isNativeExecutable(script) {
		t.Errorf("a launcher script must not read as a native executable: %q", script)
	}
	if isNativeExecutable(filepath.Join(dir, "does-not-exist")) {
		t.Error("a path that does not exist must not read as a native executable")
	}
}

// TestExeSuffixMatchesThePlatform pins the extension a native executable carries here, the
// name resolution builds its candidate paths from.
func TestExeSuffixMatchesThePlatform(t *testing.T) {
	want := ""
	if runtime.GOOS == "windows" {
		want = ".exe"
	}
	if got := exeSuffix(); got != want {
		t.Errorf("exeSuffix() = %q, want %q on %s", got, want, runtime.GOOS)
	}
}

// TestRustTargetTripleNamesThisPlatform proves the triple the vendored lookup builds its
// path from is the one this platform's binaries are published under. An unrecognized
// platform must yield the empty string, which makes the lookup fail loudly rather than
// guess at a path.
func TestRustTargetTripleNamesThisPlatform(t *testing.T) {
	got := rustTargetTriple()
	if got == "" {
		t.Skipf("no triple is published for %s/%s, which the vendored lookup reports as unusable", runtime.GOOS, runtime.GOARCH)
	}
	osPart := map[string]string{"windows": "windows", "darwin": "apple-darwin", "linux": "linux"}[runtime.GOOS]
	if osPart == "" || !strings.Contains(got, osPart) {
		t.Errorf("triple %q does not name the host OS %s", got, runtime.GOOS)
	}
	archPart := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if archPart == "" || !strings.HasPrefix(got, archPart) {
		t.Errorf("triple %q does not name the host arch %s", got, runtime.GOARCH)
	}
}
