package provision

import "github.com/ionalpha/flynn/internal/inference"

// VLLMRelease is a blessed vLLM image: the reference it is pulled by, its content
// digest, and the version that digest is. The digest is the trust anchor for the whole
// Python and CUDA stack inside it. The version is gated before the pull, so a digest
// blessed as a known-vulnerable build is refused.
type VLLMRelease struct {
	// Ref is the image reference without the digest, for diagnostics and the pull.
	Ref string
	// Digest is "sha256:" followed by 64 hex characters.
	Digest string
	// Version is the vLLM version this digest is.
	Version inference.Version
}

// vllmRelease is the pinned image. A new build is adopted by blessing its digest and
// version here after it is verified.
var vllmRelease = VLLMRelease{
	Ref:     "docker.io/vllm/vllm-openai:v0.23.0",
	Digest:  "sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f",
	Version: inference.Version{0, 23, 0},
}

// VLLMImage returns the blessed vLLM image release.
func VLLMImage() VLLMRelease { return vllmRelease }

// Gate returns the advisory-floor verdict for this image's version, so a caller
// refuses a known-vulnerable image before pulling it.
func (r VLLMRelease) Gate() error { return inference.SafeToRun("vllm", r.Version) }

// Provisioner builds the container provisioner for this image, pulling by digest
// through pull. A nil pull drives an image the engine already has.
func (r VLLMRelease) Provisioner(pull ImagePuller) ContainerProvisioner {
	return ContainerProvisioner{
		RuntimeName: "vllm",
		Version:     r.Version,
		Ref:         r.Ref,
		Digest:      r.Digest,
		Pull:        pull,
	}
}
