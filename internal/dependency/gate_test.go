package dependency

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/ionalpha/flynn/internal/inference"
	"github.com/ionalpha/flynn/resource"
)

// validHex reports whether s is a 64-character hex sha256 digest.
func validHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// TestCatalogGate is the build-time guarantee: every shipped dependency spec admits against
// the kind schema and is structurally installable. A broken official spec fails here, in
// CI, not at a user's first install.
func TestCatalogGate(t *testing.T) {
	entries, err := Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the dependency catalog is empty")
	}

	reg := resource.NewRegistry()
	if err := RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	store := resource.NewMemory(reg)

	for _, e := range entries {
		// Admission: the kind schema must accept the spec bytes as shipped.
		if _, err := store.Put(context.Background(), resource.Resource{
			APIVersion: GroupVersion, Kind: Kind, Name: e.Name, Spec: e.Raw,
		}); err != nil {
			t.Errorf("official dependency %q is not admitted: %v", e.Name, err)
			continue
		}
		if len(e.Spec.Binaries) == 0 {
			t.Errorf("official dependency %q declares no binary", e.Name)
		}
		// A pinned version must be at or above the floor: the build Flynn would install must
		// never be one the spec itself calls too old.
		if e.Spec.Pin != "" && e.Spec.MinVersion != "" {
			if inference.ParseVersion(e.Spec.Pin).Less(inference.ParseVersion(e.Spec.MinVersion)) {
				t.Errorf("official dependency %q pin %s is below its minimum %s", e.Name, e.Spec.Pin, e.Spec.MinVersion)
			}
		}
		// Every release must be fetchable and verifiable: https URL, a real sha256, a known
		// archive format, and a named binary inside.
		for _, r := range e.Spec.Releases {
			where := e.Name + " " + r.GOOS + "/" + r.GOARCH
			if len(r.URL) < 8 || r.URL[:8] != "https://" {
				t.Errorf("%s release URL is not https: %q", where, r.URL)
			}
			if !validHex(r.SHA256) {
				t.Errorf("%s release sha256 is not a 64-hex digest: %q", where, r.SHA256)
			}
			if r.Archive != "zip" && r.Archive != "tar.gz" {
				t.Errorf("%s release has an unknown archive format: %q", where, r.Archive)
			}
			if r.BinName == "" {
				t.Errorf("%s release names no binary", where)
			}
			if r.SizeBytes <= 0 {
				t.Errorf("%s release has no size (needed as the download cap)", where)
			}
		}
		// A pinning dependency must ship builds for the common desktop and server platforms,
		// so provisioning is never a dead end where Flynn is likely to run.
		if len(e.Spec.Releases) > 0 {
			for _, p := range [][2]string{{"linux", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}} {
				if _, ok := e.Spec.ReleaseFor(p[0], p[1]); !ok {
					t.Errorf("official dependency %q ships no build for %s/%s", e.Name, p[0], p[1])
				}
			}
		}
	}
}
