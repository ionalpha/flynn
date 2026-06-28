package dependency

import (
	"context"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/acquire"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/inference"
)

// Prober runs a present program's version command and returns its output. It is a narrow
// boundary so the engine can be tested without executing anything and so the host can
// confine the probe: the real implementation runs the program through the sandbox, never
// spawning a process directly. A probe error means the program is absent or would not run,
// which the engine treats as "not present" rather than trusting it blindly.
type Prober interface {
	Probe(ctx context.Context, name string, args []string) (string, error)
}

// Source is where a satisfied dependency came from.
type Source string

const (
	// SourceSystem is a program already present on the host that met the floor.
	SourceSystem Source = "system"
	// SourceProvisioned is a pinned build Flynn fetched and installed.
	SourceProvisioned Source = "provisioned"
)

// Resolved is a satisfied dependency: what to run, the version in effect, and where it came
// from. For a system program the path is the program name the sandbox resolves on PATH; for
// a provisioned build it is the absolute path of the installed binary.
type Resolved struct {
	Name    string
	Path    string
	Version string
	Source  Source
}

// Report is the observed state of a dependency without changing anything: whether a usable
// build is present on the host, its version, whether it meets the floor, and whether Flynn
// could provision one for this platform if it is not.
type Report struct {
	Name         string
	Present      bool
	Path         string
	Version      string
	MeetsFloor   bool
	CanProvision bool
}

// Manager satisfies dependencies from their specs. It is detect-installed-first: a present
// build that meets the floor is used as-is; otherwise the pinned build is fetched and
// verified through the acquire layer and installed under the data directory. It holds no
// program knowledge: every program is a spec it reads.
type Manager struct {
	store   *Store
	dl      *fetch.Downloader
	dataDir string
	prober  Prober
	goos    string
	goarch  string
}

// Option configures a Manager.
type Option func(*Manager)

// WithProber sets the version-probe boundary. A nil prober means no system program can be
// verified, so every dependency is provisioned from its pinned build.
func WithProber(p Prober) Option { return func(m *Manager) { m.prober = p } }

// WithPlatform overrides the target platform (default the running GOOS/GOARCH). Tests use it
// to exercise provisioning for a specific platform.
func WithPlatform(goos, goarch string) Option {
	return func(m *Manager) {
		if goos != "" {
			m.goos = goos
		}
		if goarch != "" {
			m.goarch = goarch
		}
	}
}

// NewManager builds a dependency manager over store. dl is the verified downloader used to
// provision a missing build; dataDir is the root the install directory lives under.
func NewManager(store *Store, dl *fetch.Downloader, dataDir string, opts ...Option) *Manager {
	m := &Manager{store: store, dl: dl, dataDir: dataDir, goos: runtime.GOOS, goarch: runtime.GOARCH}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Resolve satisfies the named dependency and returns a runnable path. A present build that
// meets the floor is returned without a download; otherwise the pinned build for this
// platform is fetched, verified, and installed. It fails when the program is neither present
// nor shipped for this platform, with a message telling the operator how to proceed.
func (m *Manager) Resolve(ctx context.Context, name string) (Resolved, error) {
	dep, err := m.store.Get(ctx, name)
	if err != nil {
		return Resolved{}, err
	}
	if r, ok := m.detect(ctx, dep.Spec, name); ok {
		return r, nil
	}
	return m.provision(ctx, dep.Spec, name)
}

// Check reports a dependency's observed state without provisioning anything.
func (m *Manager) Check(ctx context.Context, name string) (Report, error) {
	dep, err := m.store.Get(ctx, name)
	if err != nil {
		return Report{}, err
	}
	_, canProvision := dep.Spec.ReleaseFor(m.goos, m.goarch)
	rep := Report{Name: name, CanProvision: canProvision}
	floor := parseFloor(dep.Spec.MinVersion)
	for _, b := range m.presentBinaries(ctx, dep.Spec) {
		rep.Present = true
		rep.Path = b.name
		rep.Version = b.ver.String()
		rep.MeetsFloor = floor == nil || !b.ver.Less(floor)
		break // report the first present binary
	}
	return rep, nil
}

// detect implements the detect-installed-first policy: the first present binary that meets
// the floor is used. A binary that cannot be probed, or is below the floor, is skipped so a
// vulnerable or unreadable system install never shadows the pinned build.
func (m *Manager) detect(ctx context.Context, spec Spec, name string) (Resolved, bool) {
	floor := parseFloor(spec.MinVersion)
	for _, b := range m.presentBinaries(ctx, spec) {
		if floor != nil && b.ver.Less(floor) {
			continue // present but below the floor; prefer the pinned build
		}
		return Resolved{Name: name, Path: b.name, Version: b.ver.String(), Source: SourceSystem}, true
	}
	return Resolved{}, false
}

// probed is a present binary and the version read from it.
type probed struct {
	name string
	ver  inference.Version
}

// presentBinaries probes each candidate binary in order and yields, lazily via a slice, the
// ones that run and print a parseable version. With no prober or no version arguments a
// present build cannot be verified through the sandbox, so none are reported and the pinned
// build is preferred. It stops at the first success for Resolve's common case, but returns a
// slice so Check and the floor-skip in detect can consider each in turn.
func (m *Manager) presentBinaries(ctx context.Context, spec Spec) []probed {
	if m.prober == nil || len(spec.VersionArgs) == 0 {
		return nil
	}
	var out []probed
	for _, b := range spec.Binaries {
		raw, err := m.prober.Probe(ctx, b, spec.VersionArgs)
		if err != nil {
			continue
		}
		v := parseVersion(raw, spec.VersionRegex)
		if len(v) == 0 {
			continue
		}
		out = append(out, probed{name: b, ver: v})
	}
	return out
}

// provision fetches and installs the pinned build for this platform, refusing a spec whose
// pin would be below its own floor, and refusing a platform the spec ships no build for.
func (m *Manager) provision(ctx context.Context, spec Spec, name string) (Resolved, error) {
	rel, ok := spec.ReleaseFor(m.goos, m.goarch)
	if !ok {
		hint := name
		if len(spec.Binaries) > 0 {
			hint = spec.Binaries[0]
		}
		return Resolved{}, fault.New(fault.Terminal, "dependency_no_build",
			"dependency: no pinned "+name+" build for "+m.goos+"/"+m.goarch+"; install "+hint+" manually and re-run")
	}
	if floor := parseFloor(spec.MinVersion); floor != nil && spec.Pin != "" {
		if inference.ParseVersion(spec.Pin).Less(floor) {
			return Resolved{}, fault.New(fault.Terminal, "dependency_pin_below_floor",
				"dependency: "+name+" pinned version "+spec.Pin+" is below its minimum "+spec.MinVersion)
		}
	}
	kind, err := archiveKind(rel.Archive)
	if err != nil {
		return Resolved{}, err
	}
	target := filepath.Join(m.dataDir, "deps", name, versionDir(spec.Pin))
	bin, _, err := acquire.InstallTo(ctx, m.dl, acquire.Release{
		URL:       rel.URL,
		SHA256:    rel.SHA256,
		SizeBytes: rel.SizeBytes,
		Archive:   kind,
		BinName:   rel.BinName,
	}, target)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Name: name, Path: bin, Version: spec.Pin, Source: SourceProvisioned}, nil
}

// parseFloor parses a minimum-version string, returning nil when no floor is set.
func parseFloor(minVer string) inference.Version {
	if strings.TrimSpace(minVer) == "" {
		return nil
	}
	return inference.ParseVersion(minVer)
}

// parseVersion extracts the version token from raw version output (capture group one of
// regex when set, else the whole output) and parses it to a comparable version.
func parseVersion(raw, regex string) inference.Version {
	token := strings.TrimSpace(raw)
	if regex != "" {
		if re, err := regexp.Compile(regex); err == nil {
			if m := re.FindStringSubmatch(raw); len(m) > 1 {
				token = m[1]
			}
		}
	}
	return inference.ParseVersion(token)
}

// archiveKind maps a spec's archive string to the acquire layer's kind.
func archiveKind(s string) (acquire.ArchiveKind, error) {
	switch s {
	case "zip":
		return acquire.ArchiveZip, nil
	case "tar.gz":
		return acquire.ArchiveTarGz, nil
	default:
		return 0, fault.New(fault.Terminal, "dependency_archive", "dependency: unknown archive kind "+s)
	}
}

// versionDir returns a filesystem-safe single path element naming the install directory for
// a pinned version, so distinct versions install side by side and never collide.
func versionDir(pin string) string {
	v := strings.TrimSpace(pin)
	if v == "" {
		return "pinned"
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == ' ' {
			return '_'
		}
		return r
	}, v)
}
