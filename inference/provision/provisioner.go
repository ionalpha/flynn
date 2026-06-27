package provision

import (
	"context"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/inference"
)

// This file generalizes acquisition. Install (the rest of this package) obtains one shape
// of runtime: a pinned binary archive. A container runtime such as vLLM is not a binary
// archive but a digest-pinned image, and a runtime already present on the host need not be
// acquired at all. So how a runtime is obtained is made an explicit, declared strategy
// behind the Provisioner boundary rather than a single hard-coded path.
//
// Every strategy holds the same two invariants the archive path established: the runtime's
// version is gated against the advisory floor before any network access, so a build that
// would be refused at run time is never fetched or pulled; and the result is reported as an
// Acquired, the one handle the launch layer consumes whether the runtime is a binary or an
// image. The acquisition action that touches the network or the host (a download, an image
// pull, a host probe) is injected, so the policy in each strategy is decided and testable
// without a runtime or a container engine present.

// Acquired is what the launch layer needs to run a runtime once it has been obtained, in
// the one shape that covers both runtime worlds. Exactly one of Binary or Image is set: a
// binary-archive runtime resolves to an executable on disk, a container runtime resolves to
// a digest-pinned image the engine runs.
type Acquired struct {
	// Runtime is the runtime this is, matching inference.Runtime.Name.
	Runtime string
	// Version is the gate-approved version that was acquired.
	Version inference.Version
	// Binary is the server executable path for a binary-archive runtime; empty for a
	// container runtime.
	Binary string
	// Image is the digest-pinned image for a container runtime; its zero value means this
	// is a binary runtime.
	Image AcquiredImage
	// FromCache is true when the runtime was already present and nothing was downloaded or
	// pulled.
	FromCache bool
}

// IsContainer reports whether this resolves to a container image rather than a binary.
func (a Acquired) IsContainer() bool { return a.Image.Digest != "" }

// AcquiredImage is a digest-pinned container image a container runtime resolves to. The
// digest is the trust anchor (an image is content-addressed, so a moved tag cannot
// substitute different bytes); the ref is recorded for diagnostics. It mirrors the shape
// the sandbox container tier validates again before it runs the image, so the pin is
// enforced at acquisition and re-enforced at run.
type AcquiredImage struct {
	// Ref is the image reference without the digest, e.g. "vllm/vllm-openai:v0.11.1".
	Ref string
	// Digest is the content digest the image is pinned to, "sha256:" then 64 hex chars.
	Digest string
}

// Provisioner obtains a runtime and reports what launch needs to run it. Each strategy
// gates the runtime version before any network access, so a runtime that would be refused
// at run time is never downloaded or pulled.
type Provisioner interface {
	// Runtime is the inference runtime this provisions, matching inference.Runtime.Name.
	Runtime() string
	// Acquire obtains the runtime and returns the handle launch consumes. It reuses an
	// already-present build or image rather than re-fetching where it can.
	Acquire(ctx context.Context) (Acquired, error)
}

// ArchiveProvisioner acquires a runtime shipped as a pinned binary archive: the llama.cpp
// shape. It is the Provisioner adapter over Install, so the existing download-verify-extract
// path is one strategy among several rather than the only one.
type ArchiveProvisioner struct {
	// Release is the pinned build to install.
	Release Release
	// DestDir is where installed builds live.
	DestDir string
	// Downloader is the verified fetch path the archive is downloaded through.
	Downloader *fetch.Downloader
}

// Runtime is the archive's runtime name.
func (p ArchiveProvisioner) Runtime() string { return p.Release.Runtime }

// Acquire installs the release (gating it before any download) and reports its binary.
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

// ImagePuller ensures a digest-pinned image is present on the host, pulling it by digest
// if it is not. It is injected so the container acquisition policy is testable without an
// engine; the real puller runs the engine's pull, which resolves the ref to the pinned
// digest and verifies the bytes against it, so the pull is tamper-evident on its own.
type ImagePuller func(ctx context.Context, ref, digest string) error

// ContainerProvisioner acquires a runtime shipped as a container image: the vLLM shape. A
// blessed image pinned by content digest is the trust anchor, so the Python and CUDA
// contents that are awkward to verify piecemeal are sealed inside one digest-addressed
// artifact. Upgrading is blessing a new digest, nothing more.
//
// With Pull set the image is pulled by digest at acquisition; with Pull nil the strategy
// is the zero-cost path that drives an image the engine already has (the engine resolves
// the pinned digest on first run), so the same type serves both "obtain it" and "use what
// is present". Either way the version is gated and the digest is checked before anything
// runs.
type ContainerProvisioner struct {
	// RuntimeName is the runtime this image is, e.g. "vllm".
	RuntimeName string
	// Version is the runtime version the pinned digest corresponds to. Blessing a digest
	// asserts which version it is; that version is gated against the advisory floor, so a
	// digest for a known-vulnerable build is refused before it is pulled.
	Version inference.Version
	// Ref and Digest pin the image. Digest is required and must be a well-formed sha256.
	Ref    string
	Digest string
	// Pull ensures the image is present; nil treats the image as already present.
	Pull ImagePuller
}

// Runtime is the container runtime's name.
func (p ContainerProvisioner) Runtime() string { return p.RuntimeName }

// Acquire gates the version and the digest, ensures the image is present when a puller is
// set, and reports the digest-pinned image. It refuses before any pull when the version is
// below the floor or the image is not pinned to a well-formed digest.
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

// Locator finds a runtime already present on the host and returns a handle to it (a binary
// path) and the raw output of its version command, or ok=false when it is absent. It is
// injected so detection is testable without the real tool installed.
type Locator func(ctx context.Context) (binPath, rawVersion string, ok bool)

// DetectProvisioner drives a binary runtime already present on the host: the zero-cost fast
// path. It does not download anything; it locates the runtime, parses the version its
// command prints, and gates that version, so an already-present but vulnerable build is
// still refused. The container equivalent is a ContainerProvisioner with a nil puller, so
// this type covers the binary case.
type DetectProvisioner struct {
	// RuntimeName is the runtime to detect, matching inference.Runtime.Name; its known
	// version format is used to parse the located runtime's version output.
	RuntimeName string
	// Locate finds the present runtime and reports its raw version output.
	Locate Locator
}

// Runtime is the detected runtime's name.
func (p DetectProvisioner) Runtime() string { return p.RuntimeName }

// Acquire locates the runtime, gates its version, and reports its binary. It refuses when
// the runtime is absent or its present version is below the advisory floor.
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

// runtimeByName returns the known runtime with the given name, used to parse a detected
// runtime's version with the format that runtime prints.
func runtimeByName(name string) (inference.Runtime, bool) {
	for _, r := range inference.Runtimes() {
		if r.Name == name {
			return r, true
		}
	}
	return inference.Runtime{}, false
}

// pinnedDigest reports whether d is a well-formed "sha256:" digest with 64 hex characters,
// the form a container image must be pinned to. The container tier checks this again before
// it runs the image; gating it here refuses an unpinned image before the pull rather than
// after.
func pinnedDigest(d string) bool {
	hex, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hex) != 64 {
		return false
	}
	return strings.TrimLeft(hex, "0123456789abcdefABCDEF") == ""
}
