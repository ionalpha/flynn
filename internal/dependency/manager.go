package dependency

import (
	"context"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/acquire"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/inference"
)

// Prober runs a present program's version command and returns its output. The real
// one runs through the sandbox and never spawns a process directly, and tests run
// nothing. A probe error means absent or unusable, so the program is not trusted.
type Prober interface {
	Probe(ctx context.Context, name string, args []string) (string, error)
}

// Source is where a satisfied dependency came from.
type Source string

const (
	// SourceSystem is a program already on the host that met the floor.
	SourceSystem Source = "system"
	// SourceProvisioned is a pinned build Flynn fetched and installed.
	SourceProvisioned Source = "provisioned"
)

// Resolved is a satisfied dependency: the path to run, the version and its source.
// A system program's path is the name the sandbox resolves on PATH; a provisioned
// one is the installed binary's absolute path.
type Resolved struct {
	Name    string
	Path    string
	Version string
	Source  Source
}

// Report is a dependency's observed state: whether a usable build is present, its
// version, whether it meets the floor and whether one could be provisioned here.
type Report struct {
	Name         string
	Present      bool
	Path         string
	Version      string
	MeetsFloor   bool
	CanProvision bool
}

// Manager satisfies dependencies from their specs: a present build meeting the floor
// is used as-is, otherwise the pinned build is fetched, verified and installed under
// the data directory. Every program is a spec it reads.
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

// WithProber sets the version prober. With none no system program can be verified,
// so every dependency is provisioned.
func WithProber(p Prober) Option { return func(m *Manager) { m.prober = p } }

// WithPlatform overrides the target platform (default the running GOOS/GOARCH).
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

// NewManager builds a manager over store. dl provisions missing builds, verified;
// dataDir is the root of the install directory.
func NewManager(store *Store, dl *fetch.Downloader, dataDir string, opts ...Option) *Manager {
	m := &Manager{store: store, dl: dl, dataDir: dataDir, goos: runtime.GOOS, goarch: runtime.GOARCH}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Resolve satisfies the named dependency and returns a runnable path, downloading
// only when no present build meets the floor. It fails, telling the operator how to
// proceed, when the program is neither present nor shipped for this platform.
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

// detect returns the first present binary meeting the floor. An unprobeable or
// below-floor binary is skipped, so a vulnerable or unreadable system install never
// shadows the pinned build.
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

type probed struct {
	name string
	ver  inference.Version
}

// presentBinaries returns the candidate binaries that run and print a parseable
// version. Without a prober or version arguments nothing can be verified through
// the sandbox, so none are reported and the pinned build wins.
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

// provision installs the pinned build for this platform. It refuses a pin below the
// spec's floor and a platform the spec ships no build for.
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

// parseFloor returns nil when no floor is set.
func parseFloor(minVer string) inference.Version {
	if strings.TrimSpace(minVer) == "" {
		return nil
	}
	return inference.ParseVersion(minVer)
}

// parseVersion parses the version from raw output (capture group one of regex, else
// the whole output).
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

// versionDir names the install directory for a pin as one filesystem-safe path
// element, so versions install side by side.
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
