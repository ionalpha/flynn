package externagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/sandbox"
)

// ErrProgramNotFound reports that an external agent CLI is not installed, as opposed to
// installed but unreachable from inside the confinement. The two need different fixes
// (install it, versus grant the confined child access to it), so they are different
// errors and the onboarding message for each says something the user can act on.
var ErrProgramNotFound = errors.New("externagent: cli not found on PATH")

// Program is a resolved external agent CLI: the executable to launch and the directories
// a confined child must be able to read in order to run it.
//
// The path is always absolute and always the real executable, never a launcher script.
// A confined child gets read access to nothing outside its workspace by default, so a
// script that shells out to an interpreter would need that interpreter's directory
// granted too, and an interpreter installed under a system-owned directory cannot be
// granted at all by an unprivileged process. Resolving to the native executable removes
// the interpreter from the picture, so the only directory that has to be reachable is the
// one the CLI ships in, which lives under the user's own install tree.
type Program struct {
	// Path is the absolute path of the native executable to launch.
	Path string
	// ReadableDirs are the directories the confined child is granted read and execute
	// access on so it can load the executable and the helper binaries shipped beside it.
	ReadableDirs []string
}

// exeSuffix is the extension a native executable carries on this platform.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// rustTargetTriple is the Rust target triple naming this platform's vendored binaries.
// An external CLI distributed on npm ships one prebuilt native binary per triple and
// selects between them at run time; resolving the triple here reaches the same binary
// without running the launcher. An unrecognized platform yields the empty string, which
// makes the vendored lookup fail and fall back to reporting the CLI as unusable rather
// than guessing at a path.
func rustTargetTriple() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return "x86_64-pc-windows-msvc"
	case "windows/arm64":
		return "aarch64-pc-windows-msvc"
	case "darwin/amd64":
		return "x86_64-apple-darwin"
	case "darwin/arm64":
		return "aarch64-apple-darwin"
	case "linux/amd64":
		return "x86_64-unknown-linux-musl"
	case "linux/arm64":
		return "aarch64-unknown-linux-musl"
	default:
		return ""
	}
}

// isNativeExecutable reports whether path is a native executable rather than a launcher
// script. On Windows the extension decides it: a .cmd or .ps1 shim runs through a shell
// and a .js entry point through an interpreter, while .exe is the real thing. Elsewhere a
// script announces itself with a "#!" line, so a file that does not start with one is
// taken to be native.
func isNativeExecutable(path string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	// The path is one the host's PATH resolved, and only its first two bytes are read to
	// tell a script from a binary. Nothing here is executed and no content is returned.
	f, err := os.Open(path) //nolint:gosec // a PATH-resolved program, opened to sniff two bytes
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var head [2]byte
	if n, _ := f.Read(head[:]); n < 2 {
		return false
	}
	return string(head[:]) != "#!"
}

// LocateCodex resolves the codex CLI to the native executable to launch and the
// directories a confined child needs to read to run it. bin names the CLI (empty means
// "codex" on PATH) or gives an absolute path to it.
//
// The npm distribution installs a launcher (a .cmd shim on Windows, a symlinked .js entry
// point elsewhere) that finds and executes a vendored native binary for the host's
// platform. Launching the launcher would drag its interpreter into the confinement, so
// this resolves straight to the vendored binary. When the CLI is installed some other way
// (a package manager that puts a native binary on PATH) the resolved path is already
// native and is used as is.
//
// A CLI that is not installed yields ErrProgramNotFound. A launcher whose vendored binary
// cannot be found yields a distinct error naming the path that was looked for, because
// that is a broken or unfamiliar installation rather than a missing one.
func LocateCodex(bin string) (Program, error) {
	if bin == "" {
		bin = "codex"
	}
	resolved := bin
	if !filepath.IsAbs(bin) {
		var err error
		if resolved, err = sandbox.LookPath(bin); err != nil {
			return Program{}, fmt.Errorf("%w: %s", ErrProgramNotFound, bin)
		}
	}
	if _, err := os.Stat(resolved); err != nil {
		return Program{}, fmt.Errorf("%w: %s", ErrProgramNotFound, bin)
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		resolved = abs
	}
	if link, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = link
	}

	if isNativeExecutable(resolved) {
		return Program{Path: resolved, ReadableDirs: []string{filepath.Dir(resolved)}}, nil
	}
	return locateVendored(resolved)
}

// codexPackagePath is the npm package directory of the codex CLI, relative to the global
// bin directory the launcher shim is installed into.
var codexPackagePath = []string{"node_modules", "@openai", "codex"}

// locateVendored finds the native binary a launcher script stands for, given the resolved
// path of that script. Two installation shapes are searched: the launcher sitting in a
// global bin directory next to the package tree (the Windows shim), and the launcher
// sitting inside the package's own bin directory (the symlinked entry point elsewhere).
// The package root is returned as the readable directory rather than the binary's own,
// so the helper binaries the CLI shells out to, which ship beside it in the same tree,
// stay reachable from inside the confinement.
func locateVendored(launcher string) (Program, error) {
	triple := rustTargetTriple()
	if triple == "" {
		return Program{}, fmt.Errorf("externagent: no vendored codex binary is published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	exe := filepath.Join("vendor", triple, "codex", "codex"+exeSuffix())

	dir := filepath.Dir(launcher)
	roots := []string{filepath.Join(append([]string{dir}, codexPackagePath...)...)}
	// Walk up from the launcher looking for the package root, so an entry point that lives
	// inside the package (bin/codex.js) resolves as well as a shim that lives beside it.
	for cur := dir; ; {
		roots = append(roots, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for _, root := range roots {
		candidate := filepath.Join(root, exe)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return Program{Path: candidate, ReadableDirs: []string{root}}, nil
		}
	}
	return Program{}, fmt.Errorf("externagent: %s is a launcher script but no vendored codex binary for %s was found beside it (looked for %s)", launcher, triple, exe)
}
