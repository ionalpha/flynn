package diag

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Manifest describes a bundle: what produced it, over what window, and what it
// contains. It is the first member a reader opens, and the only one that can
// establish that the others arrived intact.
type Manifest struct {
	// BundleID uniquely names this capture.
	BundleID string `json:"bundle_id"`
	// FlynnVersion is the binary's version string, revision included when it has one.
	FlynnVersion string `json:"flynn_version"`
	// Revision is the VCS revision the binary was built from, when it is known.
	Revision string `json:"revision,omitempty"`
	// GoVersion is the Go toolchain the binary was built with. A profile is read
	// against the runtime that produced it, so this is not decoration.
	GoVersion string `json:"go_version"`
	// OS and Arch are the platform the capture ran on.
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// NumCPU is the parallelism the scheduler had, needed to read a CPU profile's
	// sample counts as a fraction of available time.
	NumCPU int `json:"num_cpu"`
	// Args is the command line, redacted: an objective and an API key both reach it.
	Args []string `json:"args,omitempty"`
	// Contention reports whether the block and mutex profiles were captured.
	Contention bool `json:"contention"`
	// SampleIntervalMs is the timeline sampler's period in milliseconds.
	SampleIntervalMs int64 `json:"sample_interval_ms"`
	// StartedAt and EndedAt bound the capture window.
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	// Annotations correlate the bundle with whatever the caller knew about the run,
	// such as its id, once that identity existed.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Members names and hashes every other file in the bundle.
	Members []Member `json:"members"`
}

// Member is one file in a bundle, with the size and digest it had when the
// manifest was written.
type Member struct {
	// Name is the member's file name, relative to the bundle directory.
	Name string `json:"name"`
	// Bytes is the member's size on disk.
	Bytes int64 `json:"bytes"`
	// SHA256 is the member's content digest, lowercase hex.
	SHA256 string `json:"sha256"`
}

// hashMembers digests every regular file directly in dir except exclude, which is
// the manifest itself. Members come back sorted by name so a manifest written
// twice over the same bundle is byte-identical.
func hashMembers(dir, exclude string) ([]Member, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("diag: read bundle dir: %w", err)
	}

	members := make([]Member, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || e.Name() == exclude {
			continue
		}
		m, err := hashMember(dir, e.Name())
		if err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return members, nil
}

// hashMember digests one member, streaming it rather than reading it whole: a CPU
// profile from a long run is not a size this process should have to hold twice. The
// name comes from reading the bundle directory this process just wrote, not from
// any caller's input.
func hashMember(dir, name string) (Member, error) {
	f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // G304: a dirent of the bundle directory this process created
	if err != nil {
		return Member{}, fmt.Errorf("diag: open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return Member{}, fmt.Errorf("diag: hash %s: %w", name, err)
	}
	return Member{Name: name, Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
