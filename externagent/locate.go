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

// ErrProgramNotFound reports that an external CLI is not installed. Installed but
// unreachable from inside the confinement is a different error, because the fix is a
// grant instead of an install.
var ErrProgramNotFound = errors.New("externagent: cli not found on PATH")

// Program is a resolved external CLI: the executable and the directories a confined
// child must read to run it. Path is always absolute and always the native executable,
// never a launcher script: a confined child reads nothing outside its workspace by
// default, and a script's interpreter under a system-owned directory cannot be granted
// by an unprivileged process. Only the CLI's own install directory then needs a grant.
type Program struct {
	// Path is the absolute path of the native executable.
	Path string
	// ReadableDirs are granted read and execute, so the child can load the executable
	// and the helper binaries shipped beside it.
	ReadableDirs []string
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// rustTargetTriple names this platform's vendored binaries. An npm-distributed CLI
// ships one prebuilt binary per triple and picks one at run time; naming it here reaches
// the same binary without running the launcher. An unknown platform returns "", so the
// lookup fails and the CLI is reported unusable instead of guessed at.
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

// isNativeExecutable reports whether path is a native executable. On Windows the
// extension decides: .cmd and .ps1 shims run through a shell, a .js entry point through
// an interpreter. Elsewhere a script starts with "#!", so any other file is native.
func isNativeExecutable(path string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	// Only the first two bytes are read; nothing is executed or returned.
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

// LocateCodex resolves the codex CLI to its native executable and the directories a
// confined child must read to run it. bin is a name (empty means "codex" on PATH) or an
// absolute path.
//
// The npm install puts a launcher on PATH (a .cmd shim on Windows, a symlinked .js
// elsewhere) that runs a vendored native binary; launching it would pull its
// interpreter into the confinement, so this resolves to the vendored binary. A native
// binary already on PATH is used as is. Not installed yields [ErrProgramNotFound]; a
// launcher whose binary cannot be found yields a distinct error naming the path tried,
// because that is a broken installation.
func LocateCodex(bin string) (Program, error) {
	return locateCLI(bin, "codex", locateVendored)
}

// locateCLI resolves bin (or onPath when empty) through PATH and any symlink, and uses
// it if native; otherwise behindLauncher finds the binary, since each CLI vendors it in
// a different place. The symlink is followed first because npm puts a symlink on PATH,
// and testing the link would read the shim's bytes.
func locateCLI(bin, onPath string, behindLauncher func(string) (Program, error)) (Program, error) {
	if bin == "" {
		bin = onPath
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
	return behindLauncher(resolved)
}

// LocateClaude resolves the claude CLI (Claude Code) to its native executable and the
// directories a confined child must read to run it. bin is a name (empty means "claude"
// on PATH) or an absolute path.
//
// Claude Code ships one self-contained binary per platform behind an npm launcher (a
// .cmd shim on Windows, an sh shim or symlinked entry point elsewhere); this resolves to
// the binary so the launcher's shell stays out of the confinement. Not installed yields
// [ErrProgramNotFound]; a launcher whose binary cannot be found yields a distinct error
// naming the path tried.
func LocateClaude(bin string) (Program, error) {
	return locateCLI(bin, "claude", locateClaudeNative)
}

// claudePackagePath is Claude Code's npm package directory, relative to the global bin
// directory holding the launcher.
var claudePackagePath = []string{"node_modules", "@anthropic-ai", "claude-code"}

// locateClaudeNative finds bin/claude in the package tree from the launcher's path, for
// a launcher beside the package (the Windows shim) or inside it (elsewhere). It grants
// the package root instead of bin/, so the per-platform helper package in the same tree
// stays reachable.
func locateClaudeNative(launcher string) (Program, error) {
	exe := filepath.Join("bin", "claude"+exeSuffix())

	dir := filepath.Dir(launcher)
	roots := []string{filepath.Join(append([]string{dir}, claudePackagePath...)...)}
	// Walk up so an entry point inside the package resolves too.
	for cur := dir; ; {
		roots = append(roots, filepath.Join(append([]string{cur}, claudePackagePath...)...))
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
	return Program{}, fmt.Errorf("externagent: %s is a launcher script but no native claude binary was found beside it (looked for %s)", launcher, exe)
}

// codexPackagePath is codex's npm package directory, relative to the global bin
// directory holding the launcher.
var codexPackagePath = []string{"node_modules", "@openai", "codex"}

// codexPlatformPkg names the per-platform npm package (`@openai/codex-linux-x64`) that
// newer codex releases vendor the binary in, under the main package's node_modules.
func codexPlatformPkg() string {
	osName := runtime.GOOS
	if osName == "windows" {
		osName = "win32"
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	return "codex-" + osName + "-" + arch
}

// locateVendored finds the vendored binary from the launcher's path, for a launcher in a
// global bin directory beside the package (the Windows shim) or in the package's own bin
// (elsewhere). It grants the package root, so helper binaries the CLI runs from the same
// tree stay reachable.
func locateVendored(launcher string) (Program, error) {
	triple := rustTargetTriple()
	if triple == "" {
		return Program{}, fmt.Errorf("externagent: no vendored codex binary is published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	exe := filepath.Join("vendor", triple, "codex", "codex"+exeSuffix())

	platformPkg := []string{"node_modules", "@openai", codexPlatformPkg()}
	dir := filepath.Dir(launcher)
	roots := []string{filepath.Join(append([]string{dir}, codexPackagePath...)...)}
	// Walk up so bin/codex.js resolves too, checking each level directly and in the
	// per-platform package.
	for cur := dir; ; {
		roots = append(roots, cur, filepath.Join(append([]string{cur}, platformPkg...)...))
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
