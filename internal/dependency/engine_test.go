package dependency

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/acquire"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/resource"
)

// TestResolveUnknownDependencyFails proves an unknown name fails at the store rather than
// provisioning something, and that Check reports the same failure without touching the host.
func TestResolveUnknownDependencyFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := NewManager(s, fetch.New(), t.TempDir(), WithProber(absent))

	if _, err := m.Resolve(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve of an unknown dependency: expected ErrNotFound, got %v", err)
	}
	if _, err := m.Check(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("check of an unknown dependency: expected ErrNotFound, got %v", err)
	}
}

// TestResolveWithNoFloorAcceptsAnyPresentVersion proves a spec with no minimum version
// imposes no floor: any present, probeable build is used as-is, however old it is, and no
// download happens (the release URL is unreachable, so reaching it would be the bug).
func TestResolveWithNoFloorAcceptsAnyPresentVersion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	spec := linuxSpec(t, "https://127.0.0.1:1/never", "00", 1)
	spec.MinVersion = "" // no floor
	if _, err := s.Put(ctx, "flyctl", spec); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, fetch.New(), t.TempDir(),
		WithProber(fakeProber{out: "flyctl v0.0.1 linux/amd64"}), WithPlatform("linux", "amd64"))

	got, err := m.Resolve(ctx, "flyctl")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != SourceSystem || got.Version != "0.0.1" {
		t.Fatalf("with no floor the present build must be used, got %+v", got)
	}
	rep, err := m.Check(ctx, "flyctl")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !rep.Present || !rep.MeetsFloor {
		t.Fatalf("with no floor a present build always meets it, got %+v", rep)
	}
}

// TestUnparseableVersionIsNotPresent proves a program that runs but prints no parseable
// version is not treated as a usable install: it cannot be checked against the floor, so it
// is reported absent and the pinned build is preferred over trusting it.
func TestUnparseableVersionIsNotPresent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, "https://x/y", "00", 1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, fetch.New(), t.TempDir(),
		WithProber(fakeProber{out: "flyctl: command shim, version unknown"}),
		WithPlatform("linux", "amd64"))

	rep, err := m.Check(ctx, "flyctl")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Present {
		t.Fatalf("a build with no parseable version must not count as present: %+v", rep)
	}
	if !rep.CanProvision {
		t.Fatalf("the spec ships a linux/amd64 build, so provisioning must be possible: %+v", rep)
	}
}

// TestNoProberMeansNoSystemBuild proves that without a version-probe boundary the engine
// verifies nothing on the host, so no system install can shadow the pinned build: the
// dependency reports absent and provisioning is the only path.
func TestNoProberMeansNoSystemBuild(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, "https://x/y", "00", 1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, fetch.New(), t.TempDir(), WithPlatform("linux", "amd64")) // no prober

	rep, err := m.Check(ctx, "flyctl")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.Present || rep.Path != "" || rep.Version != "" {
		t.Fatalf("with no prober nothing on the host is verified: %+v", rep)
	}
}

// TestCheckReportsNoBuildForAnUnsupportedPlatform proves Check tells an operator up front
// that Flynn cannot provision this program here, instead of failing only at the first
// resolve.
func TestCheckReportsNoBuildForAnUnsupportedPlatform(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, "https://x/y", "00", 1)); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, fetch.New(), t.TempDir(), WithProber(absent), WithPlatform("plan9", "mips"))

	rep, err := m.Check(ctx, "flyctl")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if rep.CanProvision {
		t.Fatalf("the spec ships no plan9/mips build: %+v", rep)
	}
}

// TestProvisionFailsOnADigestMismatch proves a release whose bytes do not match the pinned
// digest is refused: the download is verified before anything is installed, so a swapped or
// corrupted artifact never becomes a runnable binary.
func TestProvisionFailsOnADigestMismatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	url, dl, _, size := serveTarGz(t, "flyctl", "#!flyctl")
	wrong := strings.Repeat("0", 64) // a well-formed digest that the bytes do not match
	if _, err := s.Put(ctx, "flyctl", linuxSpec(t, url, wrong, size)); err != nil {
		t.Fatalf("put: %v", err)
	}
	m := NewManager(s, dl, t.TempDir(), WithProber(absent), WithPlatform("linux", "amd64"))

	got, err := m.Resolve(ctx, "flyctl")
	if err == nil {
		t.Fatal("a release whose digest does not match must be refused")
	}
	if got.Path != "" {
		t.Fatalf("a refused install must yield no path, got %+v", got)
	}
}

// TestProvisionRefusesAnUnknownArchiveKind proves the engine refuses to install a release
// whose container format it cannot open, rather than downloading it and failing later. The
// spec is fed through a store that hands back a record the kind schema would have rejected,
// which is how a corrupted or hostile backend would look.
func TestProvisionRefusesAnUnknownArchiveKind(t *testing.T) {
	s, fs := faulty(t)
	rec := resource.Resource{
		APIVersion: GroupVersion, Kind: Kind, Name: "flyctl",
		Spec: json.RawMessage(`{"binaries":["flyctl"],"releases":[
			{"goos":"linux","goarch":"amd64","url":"https://127.0.0.1:1/never","sha256":"00","archive":"rar","binName":"flyctl"}
		]}`),
	}
	fs.getRec = &rec
	m := NewManager(s, fetch.New(), t.TempDir(), WithProber(absent), WithPlatform("linux", "amd64"))

	if _, err := m.Resolve(context.Background(), "flyctl"); err == nil {
		t.Fatal("a release in an unknown archive format must be refused")
	}
}

// TestArchiveKind proves the spec's archive strings map to the formats the acquire layer can
// open, and that anything else is refused rather than guessed at.
func TestArchiveKind(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want acquire.ArchiveKind
		ok   bool
	}{
		{"zip", acquire.ArchiveZip, true},
		{"tar.gz", acquire.ArchiveTarGz, true},
		{"rar", 0, false},
		{"", 0, false},
		{"tar", 0, false},
	} {
		got, err := archiveKind(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Fatalf("archiveKind(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
			}
			continue
		}
		if err == nil {
			t.Fatalf("archiveKind(%q) must be refused, got %v", tc.in, got)
		}
	}
}

// TestParseFloor proves an empty or blank minimum version is no floor at all (nil), while a
// set one parses to a comparable version, which is what decides whether a present build is
// used or skipped.
func TestParseFloor(t *testing.T) {
	if got := parseFloor(""); got != nil {
		t.Fatalf("an empty minimum version is no floor, got %v", got)
	}
	if got := parseFloor("   "); got != nil {
		t.Fatalf("a blank minimum version is no floor, got %v", got)
	}
	got := parseFloor("0.3.0")
	if got.String() != "0.3.0" {
		t.Fatalf("parseFloor(0.3.0) = %v", got)
	}
}
