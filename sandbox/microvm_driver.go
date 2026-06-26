package sandbox

import (
	"context"
	"fmt"
	"os"

	"github.com/ionalpha/flynn/fault"
)

// The microVM tier ships no hypervisor and no guest image: Flynn is a boundary over the
// virtualization the host already trusts. An operator names the runtime and a guest base
// image through these environment knobs, the integration contract. Until they are set the
// tier reports itself unavailable and the gate refuses untrusted work, never downgrading
// it to a weaker tier.
const (
	envRuntime = "FLYNN_MICROVM_RUNTIME" // host path to the microVM runtime binary
	envKernel  = "FLYNN_MICROVM_KERNEL"  // host path to the guest kernel
	envRootFS  = "FLYNN_MICROVM_ROOTFS"  // host path to the guest root filesystem
)

// runtimeConfig resolves the operator-named runtime binary and guest image, or reports in
// plain language what is missing. It is the configuration half of detection: a host with
// the hardware boundary but no runtime configured is genuinely unable to boot a guest, so
// it is reported unavailable rather than pretended ready.
func runtimeConfig() (rt string, img Image, detail string, ok bool) {
	rt = os.Getenv(envRuntime)
	kernel := os.Getenv(envKernel)
	rootfs := os.Getenv(envRootFS)
	switch {
	case rt == "":
		return "", Image{}, "no microVM runtime configured (set " + envRuntime + ")", false
	case !fileExists(rt):
		return "", Image{}, "configured microVM runtime " + rt + " was not found", false
	case kernel == "" || rootfs == "":
		return "", Image{}, "no guest image configured (set " + envKernel + " and " + envRootFS + ")", false
	case !fileExists(kernel):
		return "", Image{}, "configured guest kernel " + kernel + " was not found", false
	case !fileExists(rootfs):
		return "", Image{}, "configured guest root filesystem " + rootfs + " was not found", false
	}
	return rt, Image{Kernel: kernel, RootFS: rootfs}, "runtime and guest image configured", true
}

// fileExists reports whether a non-directory file exists at path. A directory is rejected:
// a runtime binary or guest image named as a directory is a misconfiguration, and catching
// it at detection gives a clear "not found" rather than an opaque failure at launch time.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path) //nolint:gosec // a read-only existence check on an operator-configured path
	return err == nil && !fi.IsDir()
}

// commandDriver is a microVM backend that drives a host runtime binary. The platform
// supplies only how to detect the hardware boundary (KVM, HVF, Hyper-V); the runtime and
// image come from the operator configuration, and Boot builds a commandMachine. This keeps
// every platform adapter to a single detection function while the boot path, the manifest,
// and the guarantees stay shared and tested once.
type commandDriver struct {
	// name identifies the backend on the spine and in errors.
	name string
	// hardware reports whether the host's virtualization boundary is present and usable,
	// independent of whether a runtime is configured.
	hardware func() Availability
}

var _ Driver = commandDriver{}

// Name identifies the backend.
func (d commandDriver) Name() string { return d.name }

// Detect reports available only when both the hardware boundary is present and a runtime
// and image are configured. Either gap yields OK=false with a reason, so an operator
// learns exactly what to enable or install and untrusted work is refused until then.
func (d commandDriver) Detect() Availability {
	if hw := d.hardware(); !hw.OK {
		return hw
	}
	_, _, detail, ok := runtimeConfig()
	return Availability{OK: ok, Detail: detail}
}

// Boot resolves the configured runtime and image and starts a confined guest. It defaults
// the spec's image to the configured base when the caller did not name one, so the common
// path (run an untrusted model in the standard guest) needs only a root and guarantees.
func (d commandDriver) Boot(_ context.Context, spec Spec) (Machine, error) {
	rt, img, detail, ok := runtimeConfig()
	if !ok {
		return nil, fault.Wrap(fault.Forbidden, "microvm_unconfigured",
			fmt.Errorf("%w: %s", ErrNoMicroVM, detail))
	}
	if spec.Image == (Image{}) {
		spec.Image = img
	}
	return newCommandMachine(rt, spec)
}
