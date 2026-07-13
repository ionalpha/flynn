package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// stagedPrefix names the temporary file the new binary is written to while it is
// being staged. It sits in the same directory as the binary it will replace, because
// the replacement is a rename, and a rename is only atomic within one filesystem.
// Staging in the system temp directory and then copying across would leave a window
// where the installed binary is half-written, which is the window this design exists
// to close.
const stagedPrefix = ".flynn-upgrade-"

// supersededSuffix marks the outgoing binary on the platforms that cannot replace a
// running executable. It is swept on the next start.
const supersededSuffix = ".superseded"

// installTarget is where an upgrade will land, resolved and vetted.
type installTarget struct {
	// Path is the real path of the running binary, with every symlink resolved. The
	// replacement happens here rather than at the link, so a package manager's symlink
	// farm keeps pointing at the file that was upgraded, and so a symlink an attacker
	// planted cannot redirect the write to a file of their choosing.
	Path string
	// Dir is Path's directory, which is where the new binary is staged.
	Dir string
	// Mode is the outgoing binary's permission bits, which the incoming one inherits.
	Mode os.FileMode
}

// resolveTarget finds the binary to replace and refuses the cases where replacing it
// is the wrong thing to do.
func resolveTarget(exe string) (installTarget, error) {
	// Resolving the symlinks first is what makes every check below a check on the file
	// that will actually be written, and not on a name that might point somewhere else
	// by the time the write happens.
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return installTarget{}, fault.Wrap(fault.Terminal, CodeInstall,
			fmt.Errorf("resolving the running binary's path: %w", err))
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return installTarget{}, fault.Wrap(fault.Terminal, CodeInstall, err)
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return installTarget{}, fault.Wrap(fault.Terminal, CodeInstall, err)
	}
	if !info.Mode().IsRegular() {
		return installTarget{}, fault.New(fault.Terminal, CodeInstall,
			"the running binary at "+resolved+" is not a regular file, so it will not be replaced")
	}

	if owner, ok := managedBy(resolved); ok {
		return installTarget{}, fault.New(fault.Terminal, CodeManaged,
			"this flynn was installed by "+owner+", which owns the file and would be left out of step with it.\n"+
				"Upgrade it the way it was installed, or install a self-managed copy somewhere you own and upgrade that.")
	}

	dir := filepath.Dir(resolved)
	// The new binary is staged in this directory and renamed over the old one, so both
	// operations need the directory, not just the file. Finding out now produces a
	// sentence the user can act on; finding out after the download produces a mess.
	if err := checkWritableDir(dir); err != nil {
		return installTarget{}, err
	}

	return installTarget{Path: resolved, Dir: dir, Mode: info.Mode().Perm()}, nil
}

// managedPaths are the install locations owned by a package manager. Writing to one
// would leave the manager's database describing a file that is no longer there, and
// the next upgrade through the manager would silently undo this one.
var managedPaths = map[string]string{
	"/nix/store":                        "Nix",
	"/snap":                             "snap",
	"/var/lib/flatpak":                  "Flatpak",
	"/usr/bin":                          "the system package manager",
	"/usr/sbin":                         "the system package manager",
	"/usr/lib":                          "the system package manager",
	"/bin":                              "the system package manager",
	"/opt/homebrew/Cellar":              "Homebrew",
	"/usr/local/Cellar":                 "Homebrew",
	"/home/linuxbrew/.linuxbrew/Cellar": "Homebrew",
	`C:\Program Files`:                  "a system installer",
	`C:\Program Files (x86)`:            "a system installer",
	`C:\ProgramData\chocolatey`:         "Chocolatey",
}

// managedBy reports the package manager that owns a path, if one does.
func managedBy(p string) (string, bool) {
	clean := filepath.Clean(p)
	for prefix, owner := range managedPaths {
		if runtime.GOOS == "windows" {
			if strings.HasPrefix(strings.ToLower(clean), strings.ToLower(prefix)+string(filepath.Separator)) {
				return owner, true
			}
			continue
		}
		if strings.HasPrefix(clean, prefix+"/") {
			return owner, true
		}
	}
	// A binary under a Go toolchain's bin directory came from `go install`, which is
	// the tool that should take it away again.
	if gobin := filepath.Join(goPath(), "bin"); gobin != "bin" && strings.HasPrefix(clean, gobin+string(filepath.Separator)) {
		return "go install", true
	}
	return "", false
}

func goPath() string {
	if p := os.Getenv("GOPATH"); p != "" {
		return filepath.Clean(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "go")
}

// checkWritableDir proves the directory can be written before anything is downloaded,
// by writing to it. Asking the operating system whether a write would be permitted is
// a question with a different answer than actually writing: the permission bits, the
// access-control list, the read-only mount, and the container's view of them all get
// a vote, and only the write consults every one of them.
func checkWritableDir(dir string) error {
	f, err := os.CreateTemp(dir, stagedPrefix+"probe-*")
	if err != nil {
		return fault.Wrap(fault.Terminal, CodePermission,
			fmt.Errorf("this flynn cannot upgrade itself: %s is not writable by this user. Re-run with the rights to write it, or install a copy somewhere you own: %w", dir, err))
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// stage writes the verified binary into the target's directory under a temporary
// name, ready to be renamed into place, and returns its path.
func (t installTarget) stage(archivePath, binaryName string) (string, error) {
	// The staged file is created by the extractor with O_EXCL, so this only reserves a
	// name that is not taken; nothing is written through this handle.
	f, err := os.CreateTemp(t.Dir, stagedPrefix+"*")
	if err != nil {
		return "", fault.Wrap(fault.Terminal, CodeInstall, err)
	}
	staged := f.Name()
	_ = f.Close()
	if err := os.Remove(staged); err != nil {
		return "", fault.Wrap(fault.Terminal, CodeInstall, err)
	}

	if err := extractBinary(archivePath, binaryName, staged, t.Mode); err != nil {
		return "", err
	}
	// A file that came out of an archive carries the archive's bits, not necessarily the
	// ones it needs to run, and a umask can take away what the extractor asked for.
	if err := os.Chmod(staged, t.Mode); err != nil {
		_ = os.Remove(staged)
		return "", fault.Wrap(fault.Terminal, CodeInstall, err)
	}
	return staged, nil
}

// SweepSuperseded removes the outgoing binaries an earlier upgrade left behind on the
// platforms that cannot delete a running executable. It is called at startup, is best
// effort, and reports how many it collected. A leftover file is untidy, not unsafe:
// nothing executes it, and the next upgrade does not read it.
func SweepSuperseded(exe string) int {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir(filepath.Dir(resolved))
	if err != nil {
		return 0
	}
	var swept int
	base := filepath.Base(resolved)
	for _, e := range entries {
		name := e.Name()
		if !supersedes(base, name) {
			continue
		}
		if os.Remove(filepath.Join(filepath.Dir(resolved), name)) == nil {
			swept++
		}
	}
	return swept
}

// supersedes reports whether name is an outgoing copy of the binary called base. The
// name is base with the suffix appended, optionally with the pid of the process that
// could not delete it in between. The boundary matters: a bare prefix test would let
// "flynn" claim "flynn-other.superseded", a neighbouring binary's leftovers, and sweep
// away a file this program does not own. That only shows on the platforms where the
// binary has no extension, since "flynn.exe" cannot prefix "flynn-other".
func supersedes(base, name string) bool {
	if !strings.HasSuffix(name, supersededSuffix) {
		return false
	}
	return name == base+supersededSuffix || strings.HasPrefix(name, base+".")
}
