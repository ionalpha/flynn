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

// Manifest describes a bundle: its producer, its window and its members. It is the
// only member that can show the others arrived intact.
type Manifest struct {
	// BundleID uniquely names this capture.
	BundleID string `json:"bundle_id"`
	// FlynnVersion is the binary's version string, with revision when known.
	FlynnVersion string `json:"flynn_version"`
	// Revision is the VCS revision the binary was built from, when it is known.
	Revision string `json:"revision,omitempty"`
	// GoVersion is the toolchain that built the binary; a profile is read against the
	// runtime that produced it.
	GoVersion string `json:"go_version"`
	// OS and Arch are the platform the capture ran on.
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// NumCPU is the scheduler's parallelism, needed to read CPU sample counts as a
	// fraction of available time.
	NumCPU int `json:"num_cpu"`
	// Args is the redacted command line: an objective and an API key both reach it.
	Args []string `json:"args,omitempty"`
	// Contention reports whether the block and mutex profiles were captured.
	Contention bool `json:"contention"`
	// SampleIntervalMs is the timeline period in milliseconds.
	SampleIntervalMs int64 `json:"sample_interval_ms"`
	// StartedAt and EndedAt bound the capture window.
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	// Annotations correlate the bundle with what the caller knew about the run.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Members names and hashes every other file.
	Members []Member `json:"members"`
}

// Member is one bundle file with its size and digest when the manifest was written.
type Member struct {
	// Name is relative to the bundle directory.
	Name string `json:"name"`
	// Bytes is the member's size on disk.
	Bytes int64 `json:"bytes"`
	// SHA256 is the content digest, lowercase hex.
	SHA256 string `json:"sha256"`
}

// hashMembers digests every regular file directly in dir except exclude (the
// manifest), sorted by name so a rewritten manifest is byte-identical.
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

// hashMember digests one member by streaming, because a long run's CPU profile is too
// big to hold twice. name comes from this process's own directory read.
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
