package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/extension"
)

// processSpec builds a spec whose only surface is a process surface carrying block.
func processSpec(t *testing.T, block extension.ProcessBlock) extension.Spec {
	t.Helper()
	raw, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	return extension.Spec{Surfaces: map[string]json.RawMessage{extension.SurfaceProcess: raw}}
}

// TestCatalogRefusesDevSource is the invariant behind every bundled process extension: an
// official spec ships inside the binary under a reserved name, so it must run signed,
// published code and nothing else. A dev source is a path to unsigned local code, and a
// release that names no asset or no version is one the resolver cannot prove the origin of.
// Both are refused when the catalog loads, so a mistake in this repository cannot become a
// user running code nobody signed.
func TestCatalogRefusesDevSource(t *testing.T) {
	cases := []struct {
		name  string
		block extension.ProcessBlock
		want  string
	}{
		{
			name:  "dev source",
			block: extension.ProcessBlock{Dev: &extension.DevSource{Path: "/tmp/token"}},
			want:  "declares a dev source",
		},
		{
			name: "dev source alongside a release",
			block: extension.ProcessBlock{
				Dev:     &extension.DevSource{Path: "/tmp/token"},
				Release: &extension.ReleaseSource{Asset: "token", Version: "v0.1.0"},
			},
			want: "declares a dev source",
		},
		{
			name:  "no source at all",
			block: extension.ProcessBlock{},
			want:  "must declare a release",
		},
		{
			name:  "release without a version",
			block: extension.ProcessBlock{Release: &extension.ReleaseSource{Asset: "token"}},
			want:  "must declare a release",
		},
		{
			name:  "release without an asset",
			block: extension.ProcessBlock{Release: &extension.ReleaseSource{Version: "v0.1.0"}},
			want:  "must declare a release",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkProcessSource("token", processSpec(t, tc.block))
			if err == nil {
				t.Fatal("the catalog should refuse this process source")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not explain the refusal (want it to mention %q)", err, tc.want)
			}
		})
	}
}

// TestCatalogAcceptsRelease is the other half: a well-formed release source is accepted,
// and a spec with no process surface at all (every API integration) is untouched by the
// check.
func TestCatalogAcceptsRelease(t *testing.T) {
	block := extension.ProcessBlock{Release: &extension.ReleaseSource{Asset: "token", Version: "v0.1.0"}}
	if err := checkProcessSource("token", processSpec(t, block)); err != nil {
		t.Fatalf("a released process source should be accepted: %v", err)
	}
	if err := checkProcessSource("github", extension.Spec{}); err != nil {
		t.Fatalf("a spec with no process surface should be accepted: %v", err)
	}
}

// TestBundledProcessExtensionsAreReleased runs the same check over what actually ships, so
// a new bundled process extension added with a dev path fails here rather than at runtime.
func TestBundledProcessExtensionsAreReleased(t *testing.T) {
	entries, err := Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	for _, e := range entries {
		if err := checkProcessSource(e.Name, e.Spec); err != nil {
			t.Errorf("bundled extension %q: %v", e.Name, err)
		}
	}
}
