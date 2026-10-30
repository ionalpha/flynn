// Package gpu detects the graphics hardware a local inference runtime could use, so the
// installer can fetch a build that actually offloads to it. A CPU-only llama.cpp build on
// a machine with a 16 GB card leaves the card idle and the model several times slower, and
// nothing in the run tells the user why.
//
// Detection asks the vendor's own tool (nvidia-smi) and reads what it prints. It never
// loads a driver library into this process and never fails a caller: a machine with no
// tool, a tool that errors, and a tool whose output is unreadable all report "no GPU
// found", which selects the CPU build. The command is injected so every shape of output is
// exercised without hardware.
//
// It is separate from internal/hardware on purpose. That package answers a capacity
// question (how much memory a model needs) and also probes container engines and system
// RAM; choosing a build at install time needs only the adapter, the newest CUDA the driver
// runs, and the compute capability, and has to read the "CUDA UMD Version" banner that
// current drivers print in place of "CUDA Version".
package gpu

import (
	"context"
	"errors"
	"os/exec" //nolint:depguard // a read-only query of the host's own graphics adapter, as internal/hardware makes; there is no agent command here for the sandbox to confine
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ionalpha/flynn/internal/inference"
)

// Info is what detection found. The zero value means no usable GPU.
type Info struct {
	// Name is the first NVIDIA adapter's marketing name, such as "NVIDIA GeForce RTX 4070 Ti".
	Name string
	// Driver is the driver version the tool reported.
	Driver string
	// CUDA is the newest CUDA version the installed driver can run, as the driver reports
	// it. A build made with a newer toolkit than this does not load. Empty when the tool
	// did not print one.
	CUDA inference.Version
	// ComputeCap is the adapter's CUDA compute capability (8.9 for an RTX 4070 Ti), or zero
	// when the driver is too old to report it. CUDA 13 builds dropped the architectures
	// below 7.5, so a card the build no longer targets must not be offered one.
	ComputeCap float64
	// Vulkan names an AMD Radeon or Intel Arc adapter found when no NVIDIA one was, the
	// hardware the Vulkan build serves. Empty otherwise.
	Vulkan string
}

// NVIDIA reports whether an NVIDIA adapter with a working driver was found.
func (i Info) NVIDIA() bool { return i.Name != "" }

// Runner runs a command and returns its standard output. It is injected so detection is
// tested against recorded tool output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// probeTimeout bounds each tool invocation: a hung driver must not hang an install.
const probeTimeout = 10 * time.Second

// ExecRunner is the Runner that runs the real command.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // a fixed vendor tool name and fixed query arguments
}

// Detect reports the GPU on this machine, or the zero Info when there is none. run nil
// uses [ExecRunner].
func Detect(ctx context.Context, run Runner) Info { return detect(ctx, run, runtime.GOOS) }

func detect(ctx context.Context, run Runner, goos string) Info {
	if run == nil {
		run = ExecRunner
	}
	if info := detectNVIDIA(ctx, run); info.NVIDIA() {
		return info
	}
	return Info{Vulkan: detectVulkanAdapter(ctx, run, goos)}
}

func detectNVIDIA(ctx context.Context, run Runner) Info {
	// The CSV query is stable across driver versions; the plain table header is the only
	// place older drivers print the CUDA version, so it is read separately.
	out, err := run(ctx, "nvidia-smi", "--query-gpu=name,driver_version,compute_cap", "--format=csv,noheader")
	if err != nil {
		// compute_cap is rejected by drivers that predate it; retry without.
		out, err = run(ctx, "nvidia-smi", "--query-gpu=name,driver_version", "--format=csv,noheader")
		if err != nil {
			return Info{}
		}
	}
	info, err := parseQuery(string(out))
	if err != nil {
		return Info{}
	}
	if table, err := run(ctx, "nvidia-smi"); err == nil {
		info.CUDA = parseCUDAVersion(string(table))
	}
	return info
}

// vulkanAdapterRE matches the adapters worth a Vulkan build: AMD Radeon and Intel Arc.
// Integrated Intel graphics are left out, since a CPU build is often as fast and does not
// depend on the driver's Vulkan support.
var vulkanAdapterRE = regexp.MustCompile(`(?i)(?:AMD Radeon|Radeon (?:RX|PRO|Pro|VII)|Intel\(R\) Arc|Intel Arc)[^\r\n]*`)

// detectVulkanAdapter lists display adapters with the platform's own tool and returns the
// first one a Vulkan build serves. Only Windows and Linux are asked: macOS builds offload
// through Metal without a variant.
func detectVulkanAdapter(ctx context.Context, run Runner, goos string) string {
	var out []byte
	var err error
	switch goos {
	case "windows":
		out, err = run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(Get-CimInstance Win32_VideoController).Name")
	case "linux":
		out, err = run(ctx, "lspci")
	default:
		return ""
	}
	if err != nil {
		return ""
	}
	return strings.TrimSpace(vulkanAdapterRE.FindString(string(out)))
}

var errNoAdapter = errors.New("gpu: no adapter in the tool output")

// parseQuery reads the first adapter from `--query-gpu=name,driver_version[,compute_cap]`
// output. A machine with several adapters reports the first; the runtime picks its own
// device and the build choice depends only on the vendor and driver.
func parseQuery(out string) (Info, error) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		if name == "" {
			continue
		}
		info := Info{Name: name, Driver: strings.TrimSpace(fields[1])}
		if len(fields) >= 3 {
			if cc, err := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64); err == nil {
				info.ComputeCap = cc
			}
		}
		return info, nil
	}
	return Info{}, errNoAdapter
}

// cudaVersionRE matches the table header's CUDA version. Current drivers print "CUDA UMD
// Version: 13.3"; older ones print "CUDA Version: 12.4".
var cudaVersionRE = regexp.MustCompile(`CUDA (?:UMD )?Version:\s*(\d+)\.(\d+)`)

// parseCUDAVersion extracts the CUDA version from the plain nvidia-smi table, or nil.
func parseCUDAVersion(table string) inference.Version {
	m := cudaVersionRE.FindStringSubmatch(table)
	if m == nil {
		return nil
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return nil
	}
	return inference.Version{major, minor}
}
