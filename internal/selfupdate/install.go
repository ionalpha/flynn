package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// stagedPrefix names the temporary file the new binary is staged in. It sits in the
// directory of the binary it replaces because the replacement is a rename, atomic only
// within one filesystem; staging in the temp directory and copying would leave the
// installed binary half-written for a while.
const stagedPrefix = ".flynn-upgrade-"

// supersededSuffix marks the outgoing binary on the platforms that cannot replace a
// running executable. It is swept on the next start.
const supersededSuffix = ".superseded"

// installTarget is where an upgrade will land, resolved and vetted.
type installTarget struct {
	// Path is the running binary's real path, every symlink resolved. Replacing it here
	// keeps a package manager's symlink farm pointing at the upgraded file, and a
	// planted symlink cannot redirect the write.
	Path string
	// Dir is Path's directory, which is where the new binary is staged.
	Dir string
	// Mode is the outgoing binary's permission bits, which the incoming one inherits.
	Mode os.FileMode
}

// resolveTarget finds the binary to replace and refuses the cases where replacing it
// is the wrong thing to do.
func resolveTarget(exe string) (installTarget, error) {
	// Symlinks are resolved first, so every check below is on the file that will be
	// written and not on a name that may point elsewhere by then.
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
	// The new binary is staged here and renamed over the old one, so both need the
	// directory writable. Checking now gives the user an actionable message before
	// anything is downloaded.
	if err := checkWritableDir(dir); err != nil {
		return installTarget{}, err
	}

	return installTarget{Path: resolved, Dir: dir, Mode: info.Mode().Perm()}, nil
}

// managedPaths are install locations a package manager owns. Writing to one leaves the
// manager's database describing a file that is gone, and the manager's next upgrade
// would undo this one.
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
	// A cask keeps the binary under Caskroom and links it into bin; the link resolves
	// here, so the Cellar entries alone miss every cask install.
	"/opt/homebrew/Caskroom":              "Homebrew",
	"/usr/local/Caskroom":                 "Homebrew",
	"/home/linuxbrew/.linuxbrew/Caskroom": "Homebrew",
	`C:\Program Files`:                    "a system installer",
	`C:\Program Files (x86)`:              "a system installer",
	`C:\ProgramData\chocolatey`:           "Chocolatey",
	`C:\ProgramData\scoop`:                "Scoop",
}

// managedBy reports the package manager that owns a path, if one does.
func managedBy(p string) (string, bool) {
	clean := filepath.Clean(p)
	for prefix, owner := range managedPaths {
		if under(clean, prefix) {
			return owner, true
		}
	}
	for _, m := range userManagedDirs() {
		// The caller resolved the binary's symlinks, so this side is resolved too: where
		// the directory is reached through a link (a symlinked home, macOS's /var through
		// /private/var) a textual prefix misses, and a missed owner means overwriting a
		// file another tool holds.
		if under(clean, resolveDir(m.dir)) {
			return m.owner, true
		}
	}
	return "", false
}

// managedDir is a package manager's install directory that depends on the user or the
// environment, so it cannot be a fixed entry in [managedPaths].
type managedDir struct{ dir, owner string }

// userManagedDirs lists the install directories, found from the home directory or the
// environment, of the package managers Flynn is published through or commonly installed
// by. A directory that comes out relative (an unset variable, no home) is dropped: under("")
// on POSIX would match every absolute path and refuse every upgrade.
func userManagedDirs() []managedDir {
	var out []managedDir
	add := func(dir, owner string) {
		if dir != "" && filepath.IsAbs(dir) {
			out = append(out, managedDir{filepath.Clean(dir), owner})
		}
	}
	// A binary under a Go toolchain's bin directory came from `go install`, which
	// should remove it.
	if gp := goPath(); gp != "" {
		add(filepath.Join(gp, "bin"), "go install")
	}
	home, _ := os.UserHomeDir()
	// Scoop runs the binary through a shim, and the running executable is the real file
	// under apps. SCOOP and SCOOP_GLOBAL move the roots.
	if root := os.Getenv("SCOOP"); root != "" {
		add(filepath.Join(root, "apps"), "Scoop")
	} else if home != "" {
		add(filepath.Join(home, "scoop", "apps"), "Scoop")
	}
	if root := os.Getenv("SCOOP_GLOBAL"); root != "" {
		add(filepath.Join(root, "apps"), "Scoop")
	}
	// WinGet unpacks a portable package for one user here and links it from
	// WinGet\Links; a machine-wide install goes under Program Files, already listed.
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		add(filepath.Join(local, "Microsoft", "WinGet", "Packages"), "WinGet")
	}
	return out
}

// under reports whether path sits inside dir. Windows paths are compared without regard to
// case, since the same directory can be spelled either way there.
func under(path, dir string) bool {
	prefix := dir + string(filepath.Separator)
	if runtime.GOOS == "windows" {
		return strings.HasPrefix(strings.ToLower(path), strings.ToLower(prefix))
	}
	return strings.HasPrefix(path, prefix)
}

// resolveDir returns dir with its symlinks resolved, or dir unchanged when that fails
// (it may not exist, and an absent directory owns nothing).
func resolveDir(dir string) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	return resolved
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

// checkWritableDir proves the directory writable before anything is downloaded, by
// writing to it. Asking whether a write is permitted can answer differently from
// writing: permission bits, the ACL, a read-only mount and the container's view all
// have a say, and only the write consults every one.
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
	// The extractor creates the staged file with O_EXCL, so this only reserves an
	// unused name; nothing is written through this handle.
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
	// An extracted file carries the archive's mode bits, and a umask can strip what the
	// extractor asked for.
	if err := os.Chmod(staged, t.Mode); err != nil {
		_ = os.Remove(staged)
		return "", fault.Wrap(fault.Terminal, CodeInstall, err)
	}
	return staged, nil
}

// SweepSuperseded removes outgoing binaries an earlier upgrade left on platforms that
// cannot delete a running executable. It runs at startup, is best effort, and reports
// how many it removed. A leftover is untidy and harmless: nothing executes or reads it.
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

// supersedes reports whether name is an outgoing copy of the binary base: base plus
// the suffix, optionally with the pid of the process that could not delete it. A bare
// prefix test would let "flynn" claim "flynn-other.superseded", a neighbouring
// binary's leftover. That can happen only where the binary has no extension, since
// "flynn.exe" cannot prefix "flynn-other".
func supersedes(base, name string) bool {
	if !strings.HasSuffix(name, supersededSuffix) {
		return false
	}
	return name == base+supersededSuffix || strings.HasPrefix(name, base+".")
}
