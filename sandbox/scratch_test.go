package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestScratchEnvReplacesHostTemp covers the rule on a POSIX host: the host's TMPDIR, which
// the baseline copies in, is replaced, an absent one is set, and a different value was
// granted and stays. Names are case-sensitive, so a lowercase gocache does not count as
// GOCACHE.
func TestScratchEnvReplacesHostTemp(t *testing.T) {
	host := func(string) string { return "/host/tmp" }
	tmp := filepath.Join("/s", scratchTempName)
	for name, tc := range map[string]struct {
		in   map[string]string
		want string
	}{
		"host value replaced": {map[string]string{"TMPDIR": "/host/tmp"}, tmp},
		"absent set":          {map[string]string{}, tmp},
		"granted kept":        {map[string]string{"TMPDIR": "/operator/tmp"}, "/operator/tmp"},
	} {
		t.Run(name, func(t *testing.T) {
			withScratchEnv(tc.in, "/s", host, "linux")
			if tc.in["TMPDIR"] != tc.want {
				t.Fatalf("TMPDIR = %q, want %q", tc.in["TMPDIR"], tc.want)
			}
			if want := filepath.Join("/s", "go-build"); tc.in["GOCACHE"] != want {
				t.Fatalf("GOCACHE = %q, want %q", tc.in["GOCACHE"], want)
			}
		})
	}
	m := map[string]string{"gocache": "/operator/cache"}
	withScratchEnv(m, "/s", host, "linux")
	if want := filepath.Join("/s", "go-build"); m["GOCACHE"] != want {
		t.Fatalf("a lowercase gocache must not satisfy GOCACHE on a case-sensitive platform: %v", m)
	}
}

// TestScratchEnvKeepsOperatorValues covers the rule on Windows: a value the operator
// granted is not replaced by the scratch default, including when the name differs only in
// case, while the host's own TEMP, which the baseline copies in, is replaced.
func TestScratchEnvKeepsOperatorValues(t *testing.T) {
	host := map[string]string{"TEMP": `C:\host\tmp`, "TMP": `C:\host\tmp`}
	m := map[string]string{
		"TEMP":    `C:\host\tmp`,
		"TMP":     `D:\operator\tmp`,
		"gocache": `D:\operator\cache`,
	}
	scratch := "scratch"
	withScratchEnv(m, scratch, func(k string) string { return host[k] }, "windows")

	if want := filepath.Join(scratch, scratchTempName); m["TEMP"] != want {
		t.Errorf("TEMP = %q, want %q in place of the host's", m["TEMP"], want)
	}
	if m["TMP"] != `D:\operator\tmp` {
		t.Errorf("operator TMP overridden: %q", m["TMP"])
	}
	if _, dup := m["GOCACHE"]; dup || m["gocache"] != `D:\operator\cache` {
		t.Errorf("operator gocache overridden or duplicated: %v", m)
	}
	if want := filepath.Join(scratch, "go-mod"); m["GOMODCACHE"] != want {
		t.Errorf("GOMODCACHE = %q, want %q", m["GOMODCACHE"], want)
	}
	if _, set := m["TMPDIR"]; set {
		t.Errorf("TMPDIR set on Windows, which reads TEMP and TMP: %v", m)
	}
}

// TestScratchRemovalFailureIsReported proves Close reports a scratch directory it could not
// delete even after making the tree writable, so a caller learns that disk was left
// behind, and that the second attempt follows the permission reset.
func TestScratchRemovalFailureIsReported(t *testing.T) {
	l := newTestLocal(t)
	dir, err := l.scratchDir()
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, scratchTempName)
	if err := os.Chmod(nested, 0o500); err != nil {
		t.Fatal(err)
	}
	orig := removeAll
	t.Cleanup(func() {
		removeAll = orig
		_ = os.Chmod(nested, 0o700)
		_ = os.RemoveAll(dir)
	})
	var calls int
	removeAll = func(string) error {
		calls++
		return errors.New("directory in use")
	}

	err = l.removeScratch()
	if err == nil || calls != 2 {
		t.Fatalf("removeScratch error = %v after %d attempts, want an error after 2", err, calls)
	}
	info, statErr := os.Stat(nested)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm()&0o200 == 0 {
		t.Fatalf("mode %v after the failed removal, want the tree made writable before the retry", info.Mode().Perm())
	}
}
