package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
)

// codeOf reports the failure code a classified error carries, so a test can assert on
// the refusal that fired rather than on the words it happened to use.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	var fe *fault.Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, which carries no failure code", err)
	}
	return fe.Code
}

// The binary an upgrade would replace has to be a real, ordinary file that this process
// can find. Every case where it is not is refused before anything is downloaded, which
// is the point of resolving the target first: a refusal that arrives after the download
// has already run is a refusal that has already touched the disk.
func TestResolveTargetRefusals(t *testing.T) {
	t.Run("a path that does not exist", func(t *testing.T) {
		_, err := resolveTarget(filepath.Join(t.TempDir(), "no-flynn-here"))
		if err == nil {
			t.Fatal("a binary that is not there resolved as an install target")
		}
		if codeOf(t, err) != CodeInstall {
			t.Fatalf("code = %q, want %q", codeOf(t, err), CodeInstall)
		}
	})

	t.Run("a directory is not a binary", func(t *testing.T) {
		_, err := resolveTarget(t.TempDir())
		if err == nil {
			t.Fatal("a directory resolved as an install target")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an install owned by go install is refused rather than trampled", func(t *testing.T) {
		gopath := t.TempDir()
		t.Setenv("GOPATH", gopath)
		bin := filepath.Join(gopath, "bin")
		if err := os.MkdirAll(bin, 0o750); err != nil {
			t.Fatal(err)
		}
		exe := filepath.Join(bin, binaryFor(runtime.GOOS))
		if err := os.WriteFile(exe, []byte("flynn"), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := resolveTarget(exe)
		if err == nil {
			t.Fatal("a binary owned by go install was accepted as an install target")
		}
		if codeOf(t, err) != CodeManaged {
			t.Fatalf("code = %q, want %q", codeOf(t, err), CodeManaged)
		}
		if !strings.Contains(err.Error(), "go install") {
			t.Fatalf("err = %v, want it to name the tool that owns the file", err)
		}
	})
}

// A binary a package manager put there is a binary the package manager owns: replacing
// it would leave the manager's database describing a file that is gone, and the next
// upgrade through the manager would silently undo this one.
func TestManagedInstallsAreRecognised(t *testing.T) {
	// The prefix table is matched case-insensitively on Windows and exactly on POSIX, so
	// the cases that can be asserted here are the ones for the platform being run.
	var managed map[string]string
	if runtime.GOOS == "windows" {
		managed = map[string]string{
			`C:\Program Files\flynn\flynn.exe`:        "a system installer",
			`c:\program files (x86)\flynn\flynn.exe`:  "a system installer",
			`C:\ProgramData\chocolatey\bin\flynn.exe`: "Chocolatey",
		}
	} else {
		managed = map[string]string{
			"/usr/bin/flynn":                   "the system package manager",
			"/usr/local/Cellar/flynn/1/flynn":  "Homebrew",
			"/nix/store/abc-flynn-1.0.0/flynn": "Nix",
			"/snap/flynn/current/bin/flynn":    "snap",
			"/var/lib/flatpak/app/flynn/flynn": "Flatpak",
		}
	}
	for path, owner := range managed {
		got, ok := managedBy(path)
		if !ok {
			t.Errorf("managedBy(%q) did not recognise the owner", path)
			continue
		}
		if got != owner {
			t.Errorf("managedBy(%q) = %q, want %q", path, got, owner)
		}
	}

	// A path nobody owns is a path this binary may upgrade in place.
	unowned := filepath.Join(t.TempDir(), binaryFor(runtime.GOOS))
	if owner, ok := managedBy(unowned); ok {
		t.Errorf("managedBy(%q) claimed it is owned by %q", unowned, owner)
	}
}

// With no GOPATH set, the Go toolchain's bin directory is derived from the home
// directory, and that derived path is still refused. An empty GOPATH must not turn the
// check off, which is what a bare prefix comparison against "" would do: every path
// starts with the empty string.
func TestTheGoBinDirectoryIsFoundWithoutGOPATH(t *testing.T) {
	t.Setenv("GOPATH", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory on this machine: %v", err)
	}
	if owner, ok := managedBy(filepath.Join(home, "go", "bin", binaryFor(runtime.GOOS))); !ok || owner != "go install" {
		t.Fatalf("managedBy(~/go/bin/flynn) = %q, %v; want go install", owner, ok)
	}
	// And a path that merely resembles it is not swept up with it.
	if owner, ok := managedBy(filepath.Join(home, "gopher", "bin", "flynn")); ok {
		t.Fatalf("a path outside the toolchain's bin directory was claimed by %q", owner)
	}
}

// The directory is proved writable by writing to it, before anything is downloaded. A
// directory that is not there cannot be written, and saying so now produces a sentence
// the operator can act on rather than a mess after a 50MB download.
func TestCheckWritableDirRefusesADirectoryItCannotWrite(t *testing.T) {
	err := checkWritableDir(filepath.Join(t.TempDir(), "not-a-directory"))
	if err == nil {
		t.Fatal("a directory that does not exist was reported writable")
	}
	if codeOf(t, err) != CodePermission {
		t.Fatalf("code = %q, want %q", codeOf(t, err), CodePermission)
	}

	// A directory that is writable leaves nothing behind: the probe file is swept.
	dir := t.TempDir()
	if err := checkWritableDir(dir); err != nil {
		t.Fatalf("a writable directory was refused: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the writability probe left %d files behind", len(entries))
	}
}

// Staging is where the archive becomes a binary. Neither a directory that cannot hold
// the staged file nor an archive that will not open may produce a staged path the
// caller would go on to run and install.
func TestStageRefusals(t *testing.T) {
	t.Run("a directory that is not there", func(t *testing.T) {
		tgt := installTarget{Dir: filepath.Join(t.TempDir(), "gone"), Mode: 0o755}
		if _, err := tgt.stage(archiveAt(t, ".tar.gz", packArchive(t, "linux", []byte("bin"))), "flynn"); err == nil {
			t.Fatal("a binary was staged into a directory that does not exist")
		}
	})

	t.Run("an archive that does not open", func(t *testing.T) {
		dir := t.TempDir()
		tgt := installTarget{Dir: dir, Mode: 0o755}
		if _, err := tgt.stage(archiveAt(t, ".tar.gz", []byte("not an archive")), "flynn"); err == nil {
			t.Fatal("a binary was staged out of an archive that does not parse")
		}
		// And the reserved staging name was not left lying around as an empty file.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), stagedPrefix) {
				t.Errorf("a failed staging left %s behind", e.Name())
			}
		}
	})

	t.Run("a staged binary carries the outgoing binary's permissions", func(t *testing.T) {
		dir := t.TempDir()
		tgt := installTarget{Dir: dir, Mode: 0o755}
		staged, err := tgt.stage(archiveAt(t, ".tar.gz", packArchive(t, "linux", []byte("the new binary"))), "flynn")
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		body, err := os.ReadFile(staged)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "the new binary" {
			t.Fatalf("the staged file holds %q", body)
		}
		if filepath.Dir(staged) != dir {
			t.Fatalf("the binary was staged in %s, not next to the one it replaces", filepath.Dir(staged))
		}
		info, err := os.Stat(staged)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
			t.Fatalf("staged mode = %v, want the outgoing binary's 0755", info.Mode().Perm())
		}
	})
}

// The outgoing binaries an earlier upgrade could not delete (a running executable
// cannot be removed on every platform) are collected on the next start. They are inert
// while they sit there, so the only thing to prove is that they are found, that nothing
// else is, and that a missing binary does not turn the sweep into an error.
func TestSweepSuperseded(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, binaryFor(runtime.GOOS))
	keep := map[string]string{
		binaryFor(runtime.GOOS):                      "the running binary",
		"unrelated.txt":                              "not ours",
		binaryFor(runtime.GOOS) + ".superseded.keep": "the suffix is not at the end",
		"flynn-other" + supersededSuffix:             "a different binary's leftovers",
	}
	sweep := []string{
		binaryFor(runtime.GOOS) + supersededSuffix,
		binaryFor(runtime.GOOS) + ".4242" + supersededSuffix,
	}
	for name, body := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range sweep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("an outgoing binary"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if got := SweepSuperseded(exe); got != len(sweep) {
		t.Fatalf("swept %d, want %d", got, len(sweep))
	}
	for _, name := range sweep {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was not swept", name)
		}
	}
	for name := range keep {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("the sweep removed %s, which is not its to remove", name)
		}
	}

	// A binary that is not where it says it is is not a reason to fail a start.
	if got := SweepSuperseded(filepath.Join(dir, "no-such-flynn")); got != 0 {
		t.Fatalf("sweeping a path that does not exist swept %d files", got)
	}
}

// A failed replacement must never leave the binary's path empty: a failed upgrade that
// uninstalls flynn is worse than the upgrade not happening.
func TestReplacePutsTheOriginalBackWhenTheSwapFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, binaryFor(runtime.GOOS))
	if err := os.WriteFile(path, []byte("the old binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	tgt := installTarget{Path: path, Dir: dir, Mode: 0o755}

	// The staged binary is not there, so the swap fails after the running binary has
	// already been moved aside. That is the exact window the restore exists to close.
	if err := tgt.replace(filepath.Join(dir, "never-staged")); err == nil {
		t.Fatal("a replacement with nothing to install reported success")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the original binary is gone after a failed upgrade: %v", err)
	}
	if string(got) != "the old binary" {
		t.Fatalf("the binary at the install path holds %q", got)
	}
}

// TestSupersedesBoundary pins the name boundary the sweep matches on, for both shapes the
// binary takes: bare on unix, carrying an extension on windows. It runs the same cases on
// every platform, because the failure it guards only appears on one of them. A bare prefix
// test lets "flynn" claim "flynn-other.superseded" and delete a neighbouring binary's
// leftovers; with an extension, "flynn.exe" cannot prefix "flynn-other", so the same code
// looks correct on windows and is not.
func TestSupersedesBoundary(t *testing.T) {
	for _, base := range []string{"flynn", "flynn.exe"} {
		t.Run(base, func(t *testing.T) {
			swept := []string{
				base + supersededSuffix,
				base + ".4242" + supersededSuffix,
			}
			kept := []string{
				base,
				"unrelated.txt",
				base + supersededSuffix + ".keep",
				"flynn-other" + supersededSuffix,
				"flynn-other.4242" + supersededSuffix,
			}
			for _, name := range swept {
				if !supersedes(base, name) {
					t.Errorf("supersedes(%q, %q) = false, want the outgoing copy to be swept", base, name)
				}
			}
			for _, name := range kept {
				if supersedes(base, name) {
					t.Errorf("supersedes(%q, %q) = true, want a file this binary does not own to be left alone", base, name)
				}
			}
		})
	}
}
