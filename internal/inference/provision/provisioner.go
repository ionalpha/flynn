package provision

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/inference"
)

// Acquired is what the launch layer needs to run an obtained runtime, whichever way it
// was obtained. Exactly one of Binary or Image is set.
type Acquired struct {
	// Runtime matches inference.Runtime.Name.
	Runtime string
	// Version is the gate-approved version that was acquired.
	Version inference.Version
	// Binary is the server executable of a binary-archive runtime.
	Binary string
	// Image is the pinned image of a container runtime; zero for a binary runtime.
	Image AcquiredImage
	// FromCache is true when nothing was downloaded or pulled.
	FromCache bool
}

// IsContainer reports whether this resolves to a container image rather than a binary.
func (a Acquired) IsContainer() bool { return a.Image.Digest != "" }

// AcquiredImage is the digest-pinned image a container runtime resolves to. The digest
// is the trust anchor because a moved tag cannot change content-addressed bytes; Ref
// is for diagnostics. The sandbox container tier validates the same shape again before
// it runs the image.
type AcquiredImage struct {
	// Ref is the image reference without the digest, e.g. "vllm/vllm-openai:v0.11.1".
	Ref string
	// Digest is "sha256:" followed by 64 hex characters.
	Digest string
}

// Provisioner obtains a runtime and reports what launch needs to run it. Every strategy
// gates the runtime version against the advisory floor before any network access, so a
// runtime that would be refused at run time is never downloaded or pulled. The network
// or host action (download, pull, probe) is injected, so each strategy's policy is
// testable without a runtime or container engine.
type Provisioner interface {
	// Runtime matches inference.Runtime.Name.
	Runtime() string
	// Acquire obtains the runtime and returns the handle launch consumes, reusing a
	// present build or image where it can.
	Acquire(ctx context.Context) (Acquired, error)
}

// ArchiveProvisioner acquires a runtime shipped as a pinned binary archive, the
// llama.cpp shape, by delegating to [Install].
type ArchiveProvisioner struct {
	// Release is the pinned build to install.
	Release Release
	// DestDir is where installed builds live.
	DestDir string
	// Downloader is the verified fetch path for the archive.
	Downloader *fetch.Downloader
}

// Runtime is the archive's runtime name.
func (p ArchiveProvisioner) Runtime() string { return p.Release.Runtime }

// Acquire installs the release, gating it before any download, and reports its binary.
func (p ArchiveProvisioner) Acquire(ctx context.Context) (Acquired, error) {
	inst, err := Install(ctx, p.Downloader, p.Release, p.DestDir)
	if err != nil {
		return Acquired{}, err
	}
	return Acquired{
		Runtime:   p.Release.Runtime,
		Version:   inst.Version,
		Binary:    inst.BinPath,
		FromCache: inst.FromCache,
	}, nil
}

// ImagePuller ensures a digest-pinned image is present, pulling it by digest if not. The
// engine's pull verifies the bytes against the digest, so the pull is tamper-evident on
// its own.
type ImagePuller func(ctx context.Context, ref, digest string) error

// ContainerProvisioner acquires a runtime shipped as a container image, the vLLM shape.
// One digest seals the Python and CUDA stack that would be awkward to verify piecemeal,
// so upgrading is blessing a new digest. A nil Pull drives an image the engine already
// has; either way the version and the digest are checked before anything runs.
type ContainerProvisioner struct {
	// RuntimeName is the runtime this image is, e.g. "vllm".
	RuntimeName string
	// Version is the runtime version the digest is blessed as. It is gated against the
	// advisory floor, so a digest for a known-vulnerable build is refused before the pull.
	Version inference.Version
	// Ref and Digest pin the image. Digest is required and must be a well-formed sha256.
	Ref    string
	Digest string
	// Pull ensures the image is present; nil treats the image as already present.
	Pull ImagePuller
}

// Runtime is the container runtime's name.
func (p ContainerProvisioner) Runtime() string { return p.RuntimeName }

// Acquire refuses before any pull when the version is below the floor or the image is
// not pinned to a well-formed digest, then ensures the image is present.
func (p ContainerProvisioner) Acquire(ctx context.Context) (Acquired, error) {
	if err := inference.SafeToRun(p.RuntimeName, p.Version); err != nil {
		return Acquired{}, err
	}
	if !pinnedDigest(p.Digest) {
		return Acquired{}, fault.New(fault.Forbidden, "provision_unpinned_image",
			"provision: refusing a container image that is not pinned to a sha256 digest")
	}
	pulled := false
	if p.Pull != nil {
		if err := p.Pull(ctx, p.Ref, p.Digest); err != nil {
			return Acquired{}, fault.Wrap(fault.Terminal, "provision_pull", err)
		}
		pulled = true
	}
	return Acquired{
		Runtime:   p.RuntimeName,
		Version:   p.Version,
		Image:     AcquiredImage{Ref: p.Ref, Digest: p.Digest},
		FromCache: !pulled,
	}, nil
}

// Locator finds a runtime already on the host and returns its binary path and the raw
// output of its version command, or ok=false when it is absent.
type Locator func(ctx context.Context) (binPath, rawVersion string, ok bool)

// DetectProvisioner drives a binary runtime already present on the host. It downloads
// nothing, but it still gates the version the runtime prints, so a present but
// vulnerable build is refused. A ContainerProvisioner with a nil Pull is the container
// equivalent.
type DetectProvisioner struct {
	// RuntimeName matches inference.Runtime.Name, whose version format parses the
	// located runtime's output.
	RuntimeName string
	// Locate finds the present runtime and reports its raw version output.
	Locate Locator
}

// Runtime is the detected runtime's name.
func (p DetectProvisioner) Runtime() string { return p.RuntimeName }

// Acquire refuses when the runtime is absent or its version is below the advisory floor.
func (p DetectProvisioner) Acquire(ctx context.Context) (Acquired, error) {
	if p.Locate == nil {
		return Acquired{}, fault.New(fault.Terminal, "provision_no_locator", "provision: detect strategy has no locator")
	}
	bin, raw, ok := p.Locate(ctx)
	if !ok {
		return Acquired{}, fault.New(fault.Forbidden, "provision_absent",
			"provision: "+p.RuntimeName+" is not present on this host")
	}
	rt, ok := runtimeByName(p.RuntimeName)
	if !ok {
		return Acquired{}, fault.New(fault.Terminal, "provision_unknown_runtime",
			"provision: "+p.RuntimeName+" is not a known runtime")
	}
	v := rt.ParseVersion(raw)
	if err := inference.SafeToRun(p.RuntimeName, v); err != nil {
		return Acquired{}, err
	}
	return Acquired{Runtime: p.RuntimeName, Version: v, Binary: bin, FromCache: true}, nil
}

func runtimeByName(name string) (inference.Runtime, bool) {
	for _, r := range inference.Runtimes() {
		if r.Name == name {
			return r, true
		}
	}
	return inference.Runtime{}, false
}

// pinnedDigest reports whether d is "sha256:" followed by 64 hex characters. The
// container tier checks this again before it runs the image; checking here refuses an
// unpinned image before the pull.
func pinnedDigest(d string) bool {
	hex, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hex) != 64 {
		return false
	}
	return strings.TrimLeft(hex, "0123456789abcdefABCDEF") == ""
}
