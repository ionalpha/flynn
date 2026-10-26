package localmodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/internal/inference"
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
}

// RuntimeInstall describes an installed runtime build.
type RuntimeInstall struct {
	// BinPath is the server executable.
	BinPath string
	// Version is the build's version.
	Version string
	// Cached is true when the build was already installed.
	Cached bool
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
		out = append(out, provision.Release{
			Runtime: r.Runtime, Version: v, GOOS: r.GOOS, GOARCH: r.GOARCH,
			URL: r.URL, SHA256: r.SHA256, SizeBytes: r.SizeBytes, Archive: kind, BinName: r.BinName,
		})
	}
	return out, nil
}

// release picks the build for this platform: from the supplied set when there is one,
// from the pins in this module otherwise.
func (h *Host) release() (provision.Release, error) {
	goos, goarch := platform()
	if len(h.releases) > 0 {
		for _, r := range h.releases {
			if r.GOOS == goos && r.GOARCH == goarch {
				return r, nil
			}
		}
	} else if r, ok := provision.ReleaseFor(Runtime, goos, goarch); ok {
		return r, nil
	}
	return provision.Release{}, fmt.Errorf("localmodel: no pinned %s build for %s/%s", Runtime, goos, goarch)
}

// EnsureRuntime installs the runtime build for this platform and returns where it is.
// The build is refused if it is below the advisory floor, before anything is downloaded,
// and a build already installed is held to the same floor. The archive must match its
// pinned digest. The server binary's own hash is recorded when the verified archive is
// unpacked and checked again by [Host.Serve] before each launch. A build found installed
// without that record is refused, not recorded: a hash taken from a file nobody vouched
// for would bless whatever is there.
func (h *Host) EnsureRuntime(ctx context.Context) (RuntimeInstall, error) {
	rel, err := h.release()
	if err != nil {
		return RuntimeInstall{}, err
	}
	if err := h.gate(rel.Version); err != nil {
		return RuntimeInstall{}, h.fail(err)
	}
	inst, err := provision.Install(ctx, h.dl, rel, h.dataDirPath("runtimes"))
	if err != nil {
		return RuntimeInstall{}, h.fail(err)
	}
	sidecar := inst.BinPath + ".sha256"
	if inst.FromCache {
		if err := verifyRuntimeBinary(inst.BinPath); err != nil {
			return RuntimeInstall{}, err
		}
	} else {
		h.say("installed %s %s (verified)", Runtime, inst.Version)
		if err := recordRuntimeBinary(inst.BinPath, sidecar); err != nil {
			return RuntimeInstall{}, err
		}
	}
	return RuntimeInstall{BinPath: inst.BinPath, Version: inst.Version.String(), Cached: inst.FromCache}, nil
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
