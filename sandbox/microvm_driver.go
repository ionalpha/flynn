package sandbox

import (
	"context"
	"fmt"
	"os"

	"github.com/ionalpha/flynn/fault"
)

// The microVM tier ships no hypervisor or guest image; the operator names them
// here. Until set, the tier is unavailable and the gate refuses untrusted work
// rather than downgrade it.
const (
	envRuntime = "FLYNN_MICROVM_RUNTIME" // host path to the microVM runtime binary
	envKernel  = "FLYNN_MICROVM_KERNEL"  // host path to the guest kernel
	envRootFS  = "FLYNN_MICROVM_ROOTFS"  // host path to the guest root filesystem
)

// runtimeConfig returns detail naming what is missing, because hardware without a
// configured runtime cannot boot a guest.
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

// fileExists rejects a directory, so a misconfigured path fails at detection
// instead of at launch.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path) //nolint:gosec // a read-only existence check on an operator-configured path
	return err == nil && !fi.IsDir()
}

// commandDriver drives a host runtime binary. A platform supplies only hardware
// detection (KVM, HVF, Hyper-V), so the boot path is shared and tested once.
type commandDriver struct {
	// name identifies the backend on the spine and in errors.
	name string
	// hardware ignores whether a runtime is configured.
	hardware func() Availability
}

var _ Driver = commandDriver{}

// Name identifies the backend.
func (d commandDriver) Name() string { return d.name }

// Detect requires both hardware and a configured runtime and image; a gap gives
// OK=false with the reason, so the operator knows what to install.
func (d commandDriver) Detect() Availability {
	if hw := d.hardware(); !hw.OK {
		return hw
	}
	_, _, detail, ok := runtimeConfig()
	return Availability{OK: ok, Detail: detail}
}

// Boot uses the configured base image when spec names none.
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
