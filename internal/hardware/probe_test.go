package hardware

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// errNotInstalled stands in for the error a missing tool produces: exec.LookPath and
// exec.Command both fail that way on a machine without it.
var errNotInstalled = errors.New("executable file not found in $PATH")

// fakeHost describes the machine the probes see: the tools on PATH and the output each
// one prints. A tool absent from cmdOut fails as if it were not installed.
type fakeHost struct {
	// onPath is the set of executables exec.LookPath should find.
	onPath map[string]bool
	// cmdOut is the stdout keyed by the command name the probe invokes.
	cmdOut map[string]string
	// cmdErr is the set of command names that run but exit non-zero (a present tool with
	// no daemon behind it, which is the docker-installed-but-not-running case).
	cmdErr map[string]bool
	// calls records every command name the probes ran, in order.
	calls []string
}

// install points the package's probe variables at h for the duration of the test and
// restores the real ones afterwards. The variables are package-level, so a test using it
// must not run in parallel with another one that does.
func (h *fakeHost) install(t *testing.T) {
	t.Helper()
	oldRun, oldLook := runCommand, lookPath
	t.Cleanup(func() { runCommand, lookPath = oldRun, oldLook })

	runCommand = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		h.calls = append(h.calls, name)
		if h.cmdErr[name] {
			return nil, errors.New("exit status 1")
		}
		out, ok := h.cmdOut[name]
		if !ok {
			return nil, errNotInstalled
		}
		return []byte(out), nil
	}
	lookPath = func(file string) (string, error) {
		if h.onPath[file] {
			return "/usr/bin/" + file, nil
		}
		return "", errNotInstalled
	}
}

const smiHeader = `+-----------------------------------------------------------------------------+
| NVIDIA-SMI 550.54.14    Driver Version: 550.54.14    CUDA Version: 12.4     |
+-----------------------------------------------------------------------------+`

// TestDetectFullyEquippedBox checks the whole probe composes: the per-GPU query, the
// banner's CUDA version, the architecture derived from the compute capability, and the
// container tooling all land on one Box.
func TestDetectFullyEquippedBox(t *testing.T) {
	h := &fakeHost{
		onPath: map[string]bool{"docker": true, "nvidia-ctk": true},
		cmdOut: map[string]string{"nvidia-smi": "24564, NVIDIA GeForce RTX 4090, 8.9, 550.54.14\n" + smiHeader},
	}
	h.install(t)

	b := Detect(context.Background())
	if b.GPUName != "NVIDIA GeForce RTX 4090" || b.VRAMBytes != 24564*1024*1024 {
		t.Fatalf("gpu = %q / %d bytes", b.GPUName, b.VRAMBytes)
	}
	if b.ComputeCapability != "8.9" || b.GPUArch != "Ada Lovelace" || b.DriverVersion != "550.54.14" {
		t.Fatalf("cc=%q arch=%q driver=%q", b.ComputeCapability, b.GPUArch, b.DriverVersion)
	}
	if b.CUDAVersion != "12.4" {
		t.Fatalf("CUDAVersion = %q, want 12.4", b.CUDAVersion)
	}
	if !b.HasGPU() {
		t.Fatal("HasGPU() = false on a box with 24 GiB of VRAM")
	}
	if b.SupportsNVFP4() {
		t.Fatal("SupportsNVFP4() = true on Ada Lovelace, which cannot serve it")
	}
	if !b.Containers.GPUPassthrough() {
		t.Fatalf("GPUPassthrough() = false with docker + nvidia-ctk: %+v", b.Containers)
	}
	// The toolkit binary is on PATH, so the engine must not be asked as well.
	for _, c := range h.calls {
		if c == "docker" {
			t.Fatal("queried the engine for its runtimes although nvidia-ctk is on PATH")
		}
	}
}

// TestDetectBareBox checks a machine with no NVIDIA tool and no container client reports
// everything unknown rather than guessing, which is what routes a caller to an explicit
// budget. Only the RAM total, which comes from the OS and not from a tool, stays real.
func TestDetectBareBox(t *testing.T) {
	h := &fakeHost{}
	h.install(t)

	b := Detect(context.Background())
	if b.HasGPU() || b.GPUName != "" || b.ComputeCapability != "" || b.GPUArch != "" {
		t.Fatalf("detected a GPU on a box with no nvidia-smi: %+v", b)
	}
	if b.CUDAVersion != "" || b.DriverVersion != "" {
		t.Fatalf("detected driver metadata with no nvidia-smi: %+v", b)
	}
	if b.Containers.Available() || b.Containers.NVIDIAToolkit {
		t.Fatalf("detected container tooling on a bare box: %+v", b.Containers)
	}
}

// TestDetectGarbageGPUOutputIsIgnored checks a tool that runs but prints something the
// parser cannot use leaves the GPU fields empty instead of half-populated.
func TestDetectGarbageGPUOutputIsIgnored(t *testing.T) {
	h := &fakeHost{cmdOut: map[string]string{"nvidia-smi": "Failed to initialize NVML: Driver/library version mismatch\n"}}
	h.install(t)

	b := Detect(context.Background())
	if b.HasGPU() || b.GPUName != "" || b.GPUArch != "" || b.CUDAVersion != "" {
		t.Fatalf("unusable nvidia-smi output produced %+v, want all GPU fields empty", b)
	}
}

// TestDetectContainers covers the toolkit-detection rules: a host toolkit binary, the
// engine reporting an nvidia runtime when no host binary exists (the Docker Desktop case),
// and the cases where passthrough must stay off.
func TestDetectContainers(t *testing.T) {
	const withNVIDIA = `{"nvidia":{"path":"nvidia-container-runtime"},"runc":{"path":"runc"}}`
	const withoutNVIDIA = `{"runc":{"path":"runc"}}`

	cases := []struct {
		name           string
		onPath         []string
		cmdOut         map[string]string
		cmdErr         []string
		want           ContainerSupport
		wantSkipEngine bool
	}{
		{
			name:           "nothing installed",
			want:           ContainerSupport{},
			wantSkipEngine: true,
		},
		{
			name:           "docker with host toolkit binary",
			onPath:         []string{"docker", "nvidia-container-runtime"},
			want:           ContainerSupport{Docker: true, NVIDIAToolkit: true},
			wantSkipEngine: true,
		},
		{
			name:   "docker desktop: toolkit only inside the engine",
			onPath: []string{"docker"},
			cmdOut: map[string]string{"docker": withNVIDIA},
			want:   ContainerSupport{Docker: true, NVIDIAToolkit: true},
		},
		{
			name:   "docker with no nvidia runtime registered",
			onPath: []string{"docker"},
			cmdOut: map[string]string{"docker": withoutNVIDIA},
			want:   ContainerSupport{Docker: true},
		},
		{
			name:   "podman engine reports nvidia",
			onPath: []string{"podman"},
			cmdOut: map[string]string{"podman": withNVIDIA},
			want:   ContainerSupport{Podman: true, NVIDIAToolkit: true},
		},
		{
			name:   "both engines, only podman answers",
			onPath: []string{"docker", "podman"},
			cmdOut: map[string]string{"podman": withNVIDIA},
			cmdErr: []string{"docker"},
			want:   ContainerSupport{Docker: true, Podman: true, NVIDIAToolkit: true},
		},
		{
			name:   "engine installed but no daemon answering",
			onPath: []string{"docker"},
			cmdErr: []string{"docker"},
			want:   ContainerSupport{Docker: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHost{
				onPath: map[string]bool{},
				cmdOut: tc.cmdOut,
				cmdErr: map[string]bool{},
			}
			for _, p := range tc.onPath {
				h.onPath[p] = true
			}
			for _, e := range tc.cmdErr {
				h.cmdErr[e] = true
			}
			h.install(t)

			got := detectContainers(context.Background())
			if got != tc.want {
				t.Fatalf("detectContainers() = %+v, want %+v", got, tc.want)
			}
			if tc.wantSkipEngine && len(h.calls) != 0 {
				t.Fatalf("engine queried needlessly: %v", h.calls)
			}
		})
	}
}

// TestEngineRuntimeMatchIsCaseInsensitive checks the runtime name is matched however the
// engine cases it, since the JSON key is the engine's to choose.
func TestEngineRuntimeMatchIsCaseInsensitive(t *testing.T) {
	h := &fakeHost{
		onPath: map[string]bool{"docker": true},
		cmdOut: map[string]string{"docker": `{"NVIDIA":{"path":"/usr/bin/nvidia-container-runtime"}}`},
	}
	h.install(t)

	if !engineHasNVIDIARuntime(context.Background(), ContainerSupport{Docker: true}) {
		t.Fatal("engineHasNVIDIARuntime() = false for an engine reporting an NVIDIA runtime")
	}
	// With no engine available there is nothing to ask, so nothing is run.
	h.calls = nil
	if engineHasNVIDIARuntime(context.Background(), ContainerSupport{}) {
		t.Fatal("engineHasNVIDIARuntime() = true with no engine present")
	}
	if len(h.calls) != 0 {
		t.Fatalf("ran %v with no engine present", h.calls)
	}
}

// TestRunNvidiaSMIReportsAbsence checks the two nvidia-smi probes surface a missing or
// failing tool as (\"\", false) rather than an empty success, which is what keeps the
// GPU fields unset instead of zeroed.
func TestRunNvidiaSMIReportsAbsence(t *testing.T) {
	h := &fakeHost{}
	h.install(t)
	ctx := context.Background()

	if out, ok := runNvidiaSMI(ctx); ok || out != "" {
		t.Fatalf("runNvidiaSMI() = %q, %v with no tool installed", out, ok)
	}
	if out, ok := runNvidiaSMIHeader(ctx); ok || out != "" {
		t.Fatalf("runNvidiaSMIHeader() = %q, %v with no tool installed", out, ok)
	}

	h.cmdOut = map[string]string{"nvidia-smi": smiHeader}
	if out, ok := runNvidiaSMI(ctx); !ok || !strings.Contains(out, "CUDA Version") {
		t.Fatalf("runNvidiaSMI() = %q, %v with the tool present", out, ok)
	}
	if out, ok := runNvidiaSMIHeader(ctx); !ok || !strings.Contains(out, "CUDA Version") {
		t.Fatalf("runNvidiaSMIHeader() = %q, %v with the tool present", out, ok)
	}
}

// TestRealRunCommandRunsAProcess checks the production probe variable actually shells out,
// so the injection point in the tests above is exercising a real code path and not a stub
// that production never uses.
func TestRealRunCommandRunsAProcess(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go tool on PATH to run as a probe stand-in")
	}
	out, err := runCommand(context.Background(), "go", "env", "GOOS")
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("runCommand returned no output for `go env GOOS`")
	}
	if _, err := lookPath("definitely-not-an-installed-tool-xyz"); err == nil {
		t.Fatal("lookPath found a tool that is not installed")
	}
}

// TestHasRAM checks the memory-known predicate, which decides whether a CPU-only run is
// judged against a real total or falls back to an explicit budget.
func TestHasRAM(t *testing.T) {
	cases := []struct {
		bytes int64
		want  bool
	}{
		{0, false},
		{-1, false},
		{1, true},
		{64 << 30, true},
	}
	for _, tc := range cases {
		if got := (Box{RAMBytes: tc.bytes}).HasRAM(); got != tc.want {
			t.Fatalf("Box{RAMBytes: %d}.HasRAM() = %v, want %v", tc.bytes, got, tc.want)
		}
	}
	if (Box{}).HasGPU() {
		t.Fatal("zero Box reports a GPU")
	}
}

// TestArchForNonZeroMinorHopper checks a Hopper capability other than the exact "9.0"
// still maps to the family through the major-number rule.
func TestArchForNonZeroMinorHopper(t *testing.T) {
	if got := archForComputeCap("9.4"); got != "Hopper" {
		t.Fatalf("archForComputeCap(\"9.4\") = %q, want Hopper", got)
	}
}
