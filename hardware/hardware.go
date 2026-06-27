// Package hardware probes the local machine for the resources that decide which
// models it can run: the GPU and its memory, and the system RAM a CPU-only run draws
// on. It is a best-effort diagnostic, not part of a governed run: detection shells out
// to a vendor tool or reads a well-known OS source when present and reports nothing
// rather than guessing when it is absent, so a caller can fall back to an explicit
// budget. The parsing is pure and tested; only the probe itself touches the machine.
package hardware

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Box is what was detected about the machine. A zero field means "not detected", so a
// caller treats it as unknown rather than zero capacity.
type Box struct {
	// GPUName is the detected accelerator's name, empty if none was found.
	GPUName string
	// VRAMBytes is the detected GPU memory in bytes, 0 if none was found.
	VRAMBytes int64
	// RAMBytes is the total system memory in bytes, 0 if it could not be read. A
	// CPU-only run is bounded by this, not by VRAM, so it decides fit on the common
	// machine that has no usable GPU.
	RAMBytes int64
	// ComputeCapability is the GPU's CUDA compute capability ("8.9", "12.0"), empty when
	// no NVIDIA GPU was detected. It is the precise signal for what a quantized format
	// needs: the 4-bit floating-point path is only available from Blackwell upward.
	ComputeCapability string
	// GPUArch is the architecture family derived from the compute capability ("Ada
	// Lovelace", "Hopper", "Blackwell"), empty when unknown. It is the human-facing label
	// and the key the runtime auto-selection reads.
	GPUArch string
	// DriverVersion is the NVIDIA driver version string ("550.54.14"), empty when unknown.
	// It bounds which CUDA runtime a container can use.
	DriverVersion string
	// CUDAVersion is the maximum CUDA version the installed driver supports ("12.4"),
	// empty when unknown. A runtime image built against a newer CUDA than this will not
	// run, so it gates which runtime build is viable.
	CUDAVersion string
	// Containers is what the machine can run a containerized runtime with: which OCI
	// runtimes are present and whether GPU passthrough is wired. A GPU runtime shipped as
	// an image needs this; without it that path is refused in favor of a native binary.
	Containers ContainerSupport
}

// ContainerSupport is the OCI tooling detected on the machine, which decides whether a
// runtime shipped as a container image can be run, and whether it can reach the GPU.
type ContainerSupport struct {
	// Docker is true when a usable docker client was found.
	Docker bool
	// Podman is true when a usable podman client was found.
	Podman bool
	// NVIDIAToolkit is true when the NVIDIA Container Toolkit was found, the component
	// that lets a container see the host GPU. Without it a container runs CPU-only.
	NVIDIAToolkit bool
}

// Available reports whether any OCI runtime was found to run an image with.
func (c ContainerSupport) Available() bool { return c.Docker || c.Podman }

// GPUPassthrough reports whether a container can be given the host GPU: an OCI runtime
// plus the NVIDIA toolkit. A GPU runtime image needs both, so this gates that path.
func (c ContainerSupport) GPUPassthrough() bool { return c.Available() && c.NVIDIAToolkit }

// HasGPU reports whether a GPU with known memory was detected.
func (b Box) HasGPU() bool { return b.VRAMBytes > 0 }

// HasRAM reports whether the system memory total was read.
func (b Box) HasRAM() bool { return b.RAMBytes > 0 }

// SupportsNVFP4 reports whether the GPU can serve the 4-bit floating-point format, which
// is a Blackwell-and-up capability (compute capability major 10 or above). A model in
// that format is refused on an older GPU rather than served on a path it cannot run.
func (b Box) SupportsNVFP4() bool {
	major, ok := computeMajor(b.ComputeCapability)
	return ok && major >= 10
}

// Detect probes the machine. It is best-effort: an absent or failing probe leaves the
// corresponding field zero, never an error, so callers degrade to an explicit budget
// instead of failing. The two probes are independent: a machine with no GPU still
// reports its RAM, which is what a CPU-only run is judged against. The context bounds
// each probe so a wedged tool cannot hang the caller.
func Detect(ctx context.Context) Box {
	var b Box
	if out, ok := runNvidiaSMI(ctx); ok {
		if gpu, ok := parseNvidiaSMI(out); ok {
			b.GPUName, b.VRAMBytes = gpu.GPUName, gpu.VRAMBytes
			b.ComputeCapability, b.DriverVersion = gpu.ComputeCapability, gpu.DriverVersion
			b.GPUArch = archForComputeCap(b.ComputeCapability)
		}
	}
	if out, ok := runNvidiaSMIHeader(ctx); ok {
		b.CUDAVersion = parseCUDAVersion(out)
	}
	b.Containers = detectContainers(ctx)
	b.RAMBytes = systemRAMBytes(ctx)
	return b
}

// runNvidiaSMI queries the NVIDIA management tool for the per-GPU fields, returning false
// when the tool is missing or errors (no GPU, no driver, not installed). Memory and name
// have always been queried; compute capability and driver version are added so the box's
// architecture and CUDA reach are known, not just its size.
func runNvidiaSMI(ctx context.Context) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=memory.total,name,compute_cap,driver_version", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// runNvidiaSMIHeader runs nvidia-smi with no query, whose banner carries the maximum CUDA
// version the driver supports ("CUDA Version: 12.4"). That number is not available from
// the per-GPU query, so it is read from the default output. Returns false when the tool
// is absent or errors.
func runNvidiaSMIHeader(ctx context.Context) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi").Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// parseNvidiaSMI reads the first usable GPU row of the query output
// ("<MiB>, <name>, <compute_cap>, <driver_version>") into a Box. Memory is reported in
// mebibytes with the nounits format, so it is scaled to bytes. Older nvidia-smi builds
// or a trimmed query may print fewer columns, so the extra fields are read only when
// present. It returns false when no row yields a memory total.
func parseNvidiaSMI(out string) (Box, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		cols := strings.Split(line, ",")
		mib, err := strconv.ParseInt(strings.TrimSpace(cols[0]), 10, 64)
		if err != nil || mib <= 0 {
			continue
		}
		b := Box{VRAMBytes: mib * 1024 * 1024}
		if len(cols) > 1 {
			b.GPUName = strings.TrimSpace(cols[1])
		}
		if len(cols) > 2 {
			b.ComputeCapability = normalizeComputeCap(cols[2])
		}
		if len(cols) > 3 {
			b.DriverVersion = strings.TrimSpace(cols[3])
		}
		return b, true
	}
	return Box{}, false
}

// normalizeComputeCap trims a compute-capability field and drops a value nvidia-smi
// prints when it does not know one ("[N/A]", "[Not Supported]"), so an unknown stays
// empty rather than becoming a bogus capability.
func normalizeComputeCap(field string) string {
	cc := strings.TrimSpace(field)
	if cc == "" || strings.HasPrefix(cc, "[") {
		return ""
	}
	return cc
}

// computeMajor returns the major component of a compute-capability string ("8.9" -> 8)
// and whether it parsed. The major alone decides architecture-family questions like
// whether the 4-bit floating-point path is available.
func computeMajor(cc string) (int, bool) {
	major, _, _ := strings.Cut(cc, ".")
	n, err := strconv.Atoi(strings.TrimSpace(major))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// archForComputeCap maps a CUDA compute capability to its architecture family name. The
// mapping is by the capability NVIDIA assigns each generation; an unrecognized or empty
// capability yields an empty name so a caller treats the architecture as unknown rather
// than guessing. Consumer and datacenter Blackwell report different majors (12 and 10)
// but are the same family for runtime-selection purposes.
func archForComputeCap(cc string) string {
	switch cc {
	case "":
		return ""
	case "8.9":
		return "Ada Lovelace"
	case "9.0":
		return "Hopper"
	}
	major, ok := computeMajor(cc)
	if !ok {
		return ""
	}
	switch {
	case major >= 10:
		return "Blackwell"
	case major == 9:
		return "Hopper"
	case major == 8:
		return "Ampere"
	case major == 7:
		return "Turing"
	default:
		return ""
	}
}

// parseCUDAVersion reads the maximum supported CUDA version out of the nvidia-smi banner,
// which prints it as "CUDA Version: 12.4" near the driver version. It returns an empty
// string when the banner does not carry it.
func parseCUDAVersion(out string) string {
	const marker = "CUDA Version:"
	i := strings.Index(out, marker)
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(out[i+len(marker):])
	end := strings.IndexFunc(rest, func(r rune) bool {
		return r != '.' && (r < '0' || r > '9')
	})
	if end < 0 {
		end = len(rest)
	}
	v := rest[:end]
	if v == "" || v == "." {
		return ""
	}
	return v
}

// ociClients are the container clients detectContainers looks for, kept as data so the
// probe is a thin loop. Each names a tool and where its presence is recorded.
var ociClients = []struct {
	bin string
	set func(*ContainerSupport)
}{
	{"docker", func(c *ContainerSupport) { c.Docker = true }},
	{"podman", func(c *ContainerSupport) { c.Podman = true }},
}

// nvidiaToolkitBins are the executables the NVIDIA Container Toolkit installs; any one
// present means a container can be given the GPU.
var nvidiaToolkitBins = []string{"nvidia-ctk", "nvidia-container-runtime", "nvidia-container-toolkit"}

// detectContainers reports which OCI runtimes and GPU passthrough tooling are on PATH. It
// is presence-only and best-effort: finding the client is enough to record it, and a
// missing tool simply stays false, so the probe never blocks or errors. Whether a daemon
// actually answers is confirmed later by the path that uses it.
func detectContainers(_ context.Context) ContainerSupport {
	var c ContainerSupport
	for _, p := range ociClients {
		if _, err := exec.LookPath(p.bin); err == nil {
			p.set(&c)
		}
	}
	for _, bin := range nvidiaToolkitBins {
		if _, err := exec.LookPath(bin); err == nil {
			c.NVIDIAToolkit = true
			break
		}
	}
	return c
}

// parseMeminfo reads the total memory from the contents of /proc/meminfo on Linux. The
// "MemTotal:" line reports a count in kibibytes ("MemTotal:  16384256 kB"), which is
// scaled to bytes. It returns 0 when the line is absent or unparseable, so a caller
// treats the total as unknown rather than zero.
func parseMeminfo(contents string) int64 {
	for _, line := range strings.Split(contents, "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB"))
		kib, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil || kib <= 0 {
			return 0
		}
		return kib * 1024
	}
	return 0
}

// parseByteCount reads a plain decimal byte total, the form a sysctl query
// ("hw.memsize") prints. It tolerates surrounding whitespace and returns 0 when the
// output is not a positive integer.
func parseByteCount(out string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
