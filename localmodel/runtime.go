package localmodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/internal/inference"
	"github.com/ionalpha/flynn/internal/inference/gpu"
	"github.com/ionalpha/flynn/internal/inference/provision"
)

// runtimeRelease is one pinned build of the runtime for one platform: where its archive
// is, the digest the archive must match, and the server executable inside it. A host
// cannot supply one: the version it reports is a claim about a binary this package never
// runs, so accepting it from the caller would let a label decide the advisory gate. Tests
// supply them through a hook that is compiled only into the test binary.
type runtimeRelease struct {
	// Runtime is the runtime the build is, [Runtime] for the one this package serves.
	Runtime string
	// Version is the build's version, compared against the advisory floor.
	Version string
	// GOOS and GOARCH are the platform the build targets.
	GOOS, GOARCH string
	// URL is the https location of the archive.
	URL string
	// SHA256 is the digest the archive must match. Required.
	SHA256 string
	// SizeBytes caps the download; zero uses the downloader's default ceiling.
	SizeBytes int64
	// Zip selects the zip format; the default is a gzip-compressed tar.
	Zip bool
	// BinName is the server executable to locate in the extracted archive.
	BinName string
	// Variant names a GPU build ("cuda-13.3", "vulkan"); empty is the CPU baseline.
	Variant string
	// CUDA is the toolkit version of a CUDA variant, which makes it one.
	CUDA string
	// Companions are further archives unpacked beside the main one, pinned the same way.
	Companions []runtimeCompanion
}

// runtimeCompanion is an archive a variant needs beside its main one.
type runtimeCompanion struct {
	URL       string
	SHA256    string
	SizeBytes int64
}

// RuntimeInstall describes an installed runtime build.
type RuntimeInstall struct {
	// BinPath is the server executable.
	BinPath string
	// Version is the build's version.
	Version string
	// Cached is true when the build was already installed.
	Cached bool
	// Variant is the build flavor: "cpu", or a GPU build such as "cuda-13.3" or "vulkan".
	Variant string
}

// RuntimeStatus describes the runtime build this host would serve with, found the way
// [Host.EnsureRuntime] finds it. A command that reports whether the runtime is installed
// reads this instead of looking for a binary itself, so the report cannot disagree with
// the install.
type RuntimeStatus struct {
	// Installed is true when the build is on disk.
	Installed bool
	// BinPath is the server executable, empty when not installed.
	BinPath string
	// Version is the build's version.
	Version string
	// Variant is the build flavor: "cpu", or a GPU build such as "cuda-13.3" or "vulkan".
	Variant string
	// Variants are the builds pinned for this platform, the names [Host.InstallRuntime]
	// accepts.
	Variants []string
	// GPU names the graphics adapter detected, empty when none was.
	GPU string
	// UsesGPU is true when the build offloads to the detected adapter: a CUDA build on an
	// NVIDIA GPU, a Vulkan build on a Vulkan-capable one, or the Metal-enabled macOS build
	// on Apple silicon.
	UsesGPU bool
	// Problem is non-empty when the installed build fails its integrity check.
	Problem string
}

// pinnedReleases converts and checks the releases a test supplies. A release without a digest
// would be installed on trust, so it is refused here rather than at install.
func pinnedReleases(in []runtimeRelease) ([]provision.Release, error) {
	var out []provision.Release
	for _, r := range in {
		if r.SHA256 == "" {
			return nil, fmt.Errorf("%w: release %s %s/%s", ErrUnpinned, r.Runtime, r.GOOS, r.GOARCH)
		}
		v := inference.ParseVersion(r.Version)
		if r.Runtime != Runtime || len(v) == 0 || r.URL == "" || r.BinName == "" {
			return nil, fmt.Errorf("localmodel: release for %q needs the %s runtime, a numeric version, a url and a binary name", r.Runtime, Runtime)
		}
		kind := provision.ArchiveTarGz
		if r.Zip {
			kind = provision.ArchiveZip
		}
		rel := provision.Release{
			Runtime: r.Runtime, Version: v, GOOS: r.GOOS, GOARCH: r.GOARCH,
			URL: r.URL, SHA256: r.SHA256, SizeBytes: r.SizeBytes, Archive: kind, BinName: r.BinName,
			Variant: r.Variant,
		}
		switch {
		case r.CUDA != "":
			rel.Accel, rel.CUDA = provision.AccelCUDA, inference.ParseVersion(r.CUDA)
		case r.Variant == "vulkan":
			rel.Accel = provision.AccelVulkan
		}
		for _, c := range r.Companions {
			if c.SHA256 == "" {
				return nil, fmt.Errorf("%w: companion of release %s %s/%s", ErrUnpinned, r.Runtime, r.GOOS, r.GOARCH)
			}
			rel.Companions = append(rel.Companions, provision.Companion{URL: c.URL, SHA256: c.SHA256, SizeBytes: c.SizeBytes, Archive: kind})
		}
		out = append(out, rel)
	}
	return out, nil
}

// candidates are the builds pinned for this platform, the CPU baseline first: the supplied
// set when there is one, the pins in this module otherwise.
func (h *Host) candidates() ([]provision.Release, error) {
	goos, goarch := platform()
	var out []provision.Release
	if len(h.releases) > 0 {
		for _, r := range h.releases {
			if r.GOOS != goos || r.GOARCH != goarch {
				continue
			}
			if r.Variant == "" {
				out = append([]provision.Release{r}, out...)
			} else {
				out = append(out, r)
			}
		}
	} else {
		out = provision.ReleasesFor(Runtime, goos, goarch)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("localmodel: no pinned %s build for %s/%s", Runtime, goos, goarch)
	}
	return out, nil
}

// adapter reports the GPU on this machine. The probe runs at most once per Host, and only
// when a choice between builds depends on it.
func (h *Host) adapter(ctx context.Context) gpu.Info {
	h.gpuOnce.Do(func() {
		detect := h.detectGPU
		if detect == nil {
			detect = func(ctx context.Context) gpu.Info { return gpu.Detect(ctx, nil) }
		}
		h.gpuInfo = detect(ctx)
	})
	return h.gpuInfo
}

// bestRelease is the build that makes the most of this machine's hardware.
func (h *Host) bestRelease(ctx context.Context, rels []provision.Release) (provision.Release, error) {
	if len(rels) == 1 {
		return rels[0], nil
	}
	if r, ok := provision.Choose(rels, h.adapter(ctx)); ok {
		return r, nil
	}
	return provision.Release{}, fmt.Errorf("localmodel: no pinned %s build suits this machine", Runtime)
}

// activeFile records which variant [Host.EnsureRuntime] serves with, beside the builds.
func (h *Host) activeFile() string {
	return filepath.Join(provision.RuntimesDir(h.dataDir), Runtime, "active")
}

// release picks the build this host serves with: the variant the last install chose, else
// the CPU build already on disk (a runtime installed before variants existed keeps working
// and is not replaced by a download nobody asked for), else the best build for the
// hardware.
func (h *Host) release(ctx context.Context) (provision.Release, error) {
	rels, err := h.candidates()
	if err != nil {
		return provision.Release{}, err
	}
	if name, err := os.ReadFile(h.activeFile()); err == nil { //nolint:gosec // a path under the data directory
		if r, ok := provision.ByVariant(rels, strings.TrimSpace(string(name))); ok {
			return r, nil
		}
	}
	if base, ok := provision.ByVariant(rels, "cpu"); ok {
		if _, found := provision.Lookup(base, provision.RuntimesDir(h.dataDir)); found {
			return base, nil
		}
	}
	return h.bestRelease(ctx, rels)
}

// EnsureRuntime installs the runtime build this host serves with and returns where it is.
// That is the variant a previous install chose or, on a machine with none, the best build
// for the hardware. The build is refused if it is below the advisory floor, before
// anything is downloaded, and a build already installed is held to the same floor. Every
// archive must match its pinned digest. The server binary's own hash is recorded when the
// verified archives are unpacked and checked again by [Host.Serve] before each launch. A
// build found installed without that record is refused, not recorded: a hash taken from a
// file nobody vouched for would bless whatever is there.
func (h *Host) EnsureRuntime(ctx context.Context) (RuntimeInstall, error) {
	rel, err := h.release(ctx)
	if err != nil {
		return RuntimeInstall{}, err
	}
	return h.install(ctx, rel)
}

// InstallRuntime installs the named build of the runtime and makes it the one this host
// serves with. An empty variant picks the best build for the hardware: a CUDA build for an
// NVIDIA GPU whose driver can run it, the CPU build otherwise. A name from
// [RuntimeStatus.Variants] selects that build whatever the hardware is, so a user can
// install the CPU build on a machine with a GPU, or Vulkan on one detection passed over.
// Verification is the same as [Host.EnsureRuntime].
func (h *Host) InstallRuntime(ctx context.Context, variant string) (RuntimeInstall, error) {
	rels, err := h.candidates()
	if err != nil {
		return RuntimeInstall{}, err
	}
	var rel provision.Release
	if variant == "" {
		if rel, err = h.bestRelease(ctx, rels); err != nil {
			return RuntimeInstall{}, err
		}
	} else {
		var ok bool
		if rel, ok = provision.ByVariant(rels, variant); !ok {
			return RuntimeInstall{}, fmt.Errorf("localmodel: no %s build named %q for this platform; choose one of: %s",
				Runtime, variant, strings.Join(provision.Variants(rels), ", "))
		}
	}
	return h.install(ctx, rel)
}

func (h *Host) install(ctx context.Context, rel provision.Release) (RuntimeInstall, error) {
	if err := h.gate(rel.Version); err != nil {
		return RuntimeInstall{}, h.fail(err)
	}
	dest := provision.RuntimesDir(h.dataDir)
	if _, found := provision.Lookup(rel, dest); !found {
		h.say("downloading %s %s (%s, %s)", Runtime, rel.Version, rel.Name(), formatBytes(rel.DownloadBytes()))
	}
	inst, err := provision.Install(ctx, h.dl, rel, dest)
	if err != nil {
		return RuntimeInstall{}, h.fail(err)
	}
	sidecar := inst.BinPath + ".sha256"
	if inst.FromCache {
		if err := verifyRuntimeBinary(inst.BinPath); err != nil {
			return RuntimeInstall{}, err
		}
	} else {
		h.say("installed %s %s %s (verified)", Runtime, inst.Version, rel.Name())
		if err := recordRuntimeBinary(inst.BinPath, sidecar); err != nil {
			return RuntimeInstall{}, err
		}
	}
	if err := fsatomic.WriteFile(h.activeFile(), []byte(rel.Name()+"\n"), 0o600); err != nil {
		return RuntimeInstall{}, fmt.Errorf("localmodel: record the chosen runtime build: %w", err)
	}
	return RuntimeInstall{BinPath: inst.BinPath, Version: inst.Version.String(), Cached: inst.FromCache, Variant: rel.Name()}, nil
}

// RuntimeStatus reports the runtime build this host would serve with and whether it is
// installed. It finds the build exactly as [Host.EnsureRuntime] does, through the same
// selection and the same install location, and downloads nothing.
func (h *Host) RuntimeStatus(ctx context.Context) (RuntimeStatus, error) {
	rels, err := h.candidates()
	if err != nil {
		return RuntimeStatus{}, err
	}
	rel, err := h.release(ctx)
	if err != nil {
		return RuntimeStatus{}, err
	}
	hw := h.adapter(ctx)
	st := RuntimeStatus{
		Version: rel.Version.String(), Variant: rel.Name(), Variants: provision.Variants(rels),
		GPU: hw.Name, UsesGPU: offloads(rel, hw),
	}
	if st.GPU == "" {
		st.GPU = hw.Vulkan
	}
	inst, found := provision.Lookup(rel, provision.RuntimesDir(h.dataDir))
	if !found {
		return st, nil
	}
	st.Installed, st.BinPath = true, inst.BinPath
	if err := verifyRuntimeBinary(inst.BinPath); err != nil {
		st.Problem = err.Error()
	}
	return st, nil
}

// offloads reports whether the build rel runs on the adapter hw describes. The macOS arm64
// build is compiled with Metal, so the baseline there offloads to Apple silicon.
func offloads(rel provision.Release, hw gpu.Info) bool {
	switch rel.Accel {
	case provision.AccelCUDA:
		return hw.NVIDIA()
	case provision.AccelVulkan:
		return hw.NVIDIA() || hw.Vulkan != ""
	case provision.AccelNone:
		return rel.GOOS == "darwin" && rel.GOARCH == "arm64"
	}
	return false
}

// formatBytes renders a download size for a progress line.
func formatBytes(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", (n+1<<19)>>20)
}

// recordRuntimeBinary stores the hash of an installed server binary beside it, the
// reference [verifyRuntimeBinary] compares against. Only the account that owns the data
// directory can rewrite either file, which is what makes the comparison mean something.
func recordRuntimeBinary(bin, sidecar string) error {
	sum, err := hashFile(bin)
	if err != nil {
		return fmt.Errorf("localmodel: hash runtime binary: %w", err)
	}
	return fsatomic.WriteFile(sidecar, []byte(sum+"\n"), 0o600)
}

// verifyRuntimeBinary returns ErrDigestMismatch when the server binary no longer hashes
// to the value recorded at install.
func verifyRuntimeBinary(bin string) error {
	want, err := os.ReadFile(bin + ".sha256") //nolint:gosec // a path under the data directory
	if err != nil {
		return fmt.Errorf("localmodel: runtime binary has no recorded hash: %w", err)
	}
	got, err := hashFile(bin)
	if err != nil {
		return fmt.Errorf("localmodel: hash runtime binary: %w", err)
	}
	if got != strings.TrimSpace(string(want)) {
		return fmt.Errorf("%w: the runtime binary changed after it was installed", ErrDigestMismatch)
	}
	return nil
}

// RaiseFloor raises the minimum runtime version this Host will run, for an advisory
// learned after the module was built. It keeps the higher of the current floor and
// version, so a lower version changes nothing, and it has no inverse. The floor belongs to
// this Host: another Host in the same process does not see it, and it is not remembered
// when this one closes. Servers already running below it are stopped the next time
// [Host.Serve] or [Host.Reach] finds them. advisoryID names the advisory in a later
// refusal.
func (h *Host) RaiseFloor(version, advisoryID string) error {
	v := inference.ParseVersion(version)
	if len(v) == 0 {
		return errors.New("localmodel: floor version has no numeric component")
	}
	h.floorMu.Lock()
	defer h.floorMu.Unlock()
	if len(h.floor.version) == 0 || h.floor.version.Less(v) {
		h.floor = hostFloor{version: v, advisory: advisoryID}
	}
	return nil
}

// Floor returns the minimum version of the runtime this Host will run: the highest of the
// compiled-in floor, one the process learned from a signed advisory feed, and one raised
// on this Host.
func (h *Host) Floor() string {
	v, ok, _ := inference.FloorFor(Runtime)
	h.floorMu.RLock()
	defer h.floorMu.RUnlock()
	if len(h.floor.version) > 0 && (!ok || v.Less(h.floor.version)) {
		return h.floor.version.String()
	}
	if !ok {
		return ""
	}
	return v.String()
}

// hostFloor is a floor raised on one Host and the advisory it came from.
type hostFloor struct {
	version  inference.Version
	advisory string
}

// gate refuses a runtime build below any floor in force: the process-wide one (compiled in
// or learned from the signed feed), then this Host's own. Every path that installs or
// adopts a runtime goes through it.
func (h *Host) gate(v inference.Version) error {
	if err := inference.SafeToRun(Runtime, v); err != nil {
		return err
	}
	h.floorMu.RLock()
	f := h.floor
	h.floorMu.RUnlock()
	if len(f.version) > 0 && v.Less(f.version) {
		why := "older than the minimum this host raised"
		if f.advisory != "" {
			why = "exposed to " + f.advisory
		}
		return fault.New(fault.Forbidden, "runtime_vulnerable",
			fmt.Sprintf("inference: %s %s is unsafe: %s; update the runtime", Runtime, v, why))
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // a path under the data directory
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
