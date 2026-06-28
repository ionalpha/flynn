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

// Prober runs a present program to read its version. It is a narrow boundary so the engine
// can be tested without executing anything and so the host can confine the probe (a fixed,
// argument-only invocation of a discovered binary). A probe failure is reported, never
// fatal: a binary that will not print a version is treated as not usable rather than
// trusted blindly.
type Prober interface {
	Probe(ctx context.Context, path string, args []string) (string, error)
}

// Source is where a satisfied dependency came from.
type Source string

const (
	// SourceSystem is a program already present on the host that met the floor.
	SourceSystem Source = "system"
	// SourceProvisioned is a pinned build Flynn fetched and installed.
	SourceProvisioned Source = "provisioned"
)

// Resolved is a satisfied dependency: the path to run, the version in effect, and where it
// came from.
type Resolved struct {
	Name    string
	Path    string
	Version string
	Source  Source
}

// Report is the observed state of a dependency without changing anything, for a status
// surface: whether a usable build is present on the host, its version, whether it meets the
// floor, and whether Flynn could provision one for this platform if it is not.
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
	store    *Store
	dl       *fetch.Downloader
	dataDir  string
	prober   Prober
	lookPath func(string) (string, error)
	goos     string
	goarch   string
}

// Option configures a Manager.
type Option func(*Manager)

// WithProber sets the version-probe boundary (default a sandboxed system prober supplied by
// the caller; a nil prober means present binaries are accepted without a version check).
func WithProber(p Prober) Option { return func(m *Manager) { m.prober = p } }

// WithLookPath sets the PATH lookup used for detection (default exec.LookPath via the
// system prober's resolver). Tests inject a fake.
func WithLookPath(f func(string) (string, error)) Option {
	return func(m *Manager) {
		if f != nil {
			m.lookPath = f
		}
	}
}

// WithPlatform overrides the target platform (default the running GOOS/GOARCH). Tests use
// it to exercise the provisioning path for a specific platform.
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
	m := &Manager{
		store:    store,
		dl:       dl,
		dataDir:  dataDir,
		lookPath: defaultLookPath,
		goos:     runtime.GOOS,
		goarch:   runtime.GOARCH,
	}
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
	if r, ok := m.detect(ctx, dep.Spec, name); ok {
		rep.Present = true
		rep.Path = r.Path
		rep.Version = r.Version
		rep.MeetsFloor = true
		return rep, nil
	}
	// Detection failed the floor or found nothing; record presence at a lower fidelity so
	// the surface can say "present but below floor" versus "absent".
	for _, b := range dep.Spec.Binaries {
		if p, err := m.lookPath(b); err == nil {
			rep.Present = true
			rep.Path = p
			break
		}
	}
	return rep, nil
}

// detect implements the detect-installed-first policy: the first present binary that meets
// the floor is returned. A binary that cannot be probed, or is below the floor, is skipped
// so a vulnerable or unreadable system install never shadows the pinned build.
func (m *Manager) detect(ctx context.Context, spec Spec, name string) (Resolved, bool) {
	floor := parseFloor(spec.MinVersion)
	for _, b := range spec.Binaries {
		path, err := m.lookPath(b)
		if err != nil {
			continue
		}
		// No version probe configured, or no prober wired: accept the present binary as-is.
		if len(spec.VersionArgs) == 0 || m.prober == nil {
			return Resolved{Name: name, Path: path, Source: SourceSystem}, true
		}
		raw, err := m.prober.Probe(ctx, path, spec.VersionArgs)
		if err != nil {
			continue
		}
		v := parseVersion(raw, spec.VersionRegex)
		if floor != nil && v.Less(floor) {
			continue // present but below the floor; prefer the pinned build
		}
		return Resolved{Name: name, Path: path, Version: v.String(), Source: SourceSystem}, true
	}
	return Resolved{}, false
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
func parseFloor(min string) inference.Version {
	if strings.TrimSpace(min) == "" {
		return nil
	}
	return inference.ParseVersion(min)
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
