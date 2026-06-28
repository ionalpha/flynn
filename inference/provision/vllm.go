package provision

import "github.com/ionalpha/flynn/inference"

// This file pins the vLLM container image Flynn runs the GPU serving path on. A container
// runtime is acquired as a digest-pinned image rather than a binary archive: the digest is
// the single trust anchor for the whole Python and CUDA stack sealed inside it, the same
// content-addressed discipline the binary releases use. Upgrading the runtime is blessing a
// new digest here, nothing more.

// VLLMRelease is the blessed vLLM image: the reference it is pulled by, the content digest it
// is pinned to, and the version that digest is. The version is gated against the advisory
// floor before the image is pulled, so a digest blessed as a known-vulnerable build is
// refused, exactly as a binary Release is gated before it is fetched.
type VLLMRelease struct {
	// Ref is the image reference without the digest, for diagnostics and the pull.
	Ref string
	// Digest is the content digest the image is pinned to ("sha256:" + 64 hex).
	Digest string
	// Version is the vLLM version this digest is, gated against the inference floor.
	Version inference.Version
}

// vllmRelease is the pinned vLLM image. The digest is the trust anchor; a new build is
// adopted by blessing its digest and version here after it is verified.
var vllmRelease = VLLMRelease{
	Ref:     "docker.io/vllm/vllm-openai:v0.23.0",
	Digest:  "sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f",
	Version: inference.Version{0, 23, 0},
}

// VLLMImage returns the blessed vLLM image release.
func VLLMImage() VLLMRelease { return vllmRelease }

// Gate reports the advisory-floor error for this image's version, or nil when the version is
// safe to run, so a caller refuses a known-vulnerable image before pulling it.
func (r VLLMRelease) Gate() error { return inference.SafeToRun("vllm", r.Version) }

// Provisioner builds the container provisioner that acquires this image, pulling it by
// digest through pull (the engine verifies the bytes against the digest). A nil pull drives
// an image the engine already has.
func (r VLLMRelease) Provisioner(pull ImagePuller) ContainerProvisioner {
	return ContainerProvisioner{
		RuntimeName: "vllm",
		Version:     r.Version,
		Ref:         r.Ref,
		Digest:      r.Digest,
		Pull:        pull,
	}
}
