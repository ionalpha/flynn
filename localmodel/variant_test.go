package localmodel_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/localmodel"
)

// rtx is the adapter the CUDA tests pretend to run on: a card CUDA 13 still supports, with
// a driver new enough to load the newest pinned build.
var rtx = localmodel.NVIDIAGPU("NVIDIA GeForce RTX 4070 Ti", "610.88", 13, 3, 8.9)

// metal is true where the baseline build is compiled with Metal, so it offloads to the
// machine's own GPU and is reported as using it whatever adapter was found.
var metal = runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"

// variantWorld is a TLS origin serving a CPU build and a CUDA build with its runtime
// libraries, and the release set that pins them.
type variantWorld struct {
	t       *testing.T
	dataDir string
	binName string
	cfg     localmodel.Config
	// libs is what the origin serves for the CUDA runtime libraries; a test swaps it to
	// stand in for a tampered download.
	libs atomic.Pointer[[]byte]
	// cpuRel, cudaRel and vulkanRel are the pinned builds the origin serves. cfg pins the
	// first two; a test that wants another set builds its own with [variantWorld.config].
	cpuRel, cudaRel, vulkanRel localmodel.Release
}

// config is a Config that installs from exactly the releases given.
func (w *variantWorld) config(rels ...localmodel.Release) localmodel.Config {
	return localmodel.WithReleases(localmodel.Config{
		DataDir: w.dataDir, HTTPClient: w.cfg.HTTPClient, MinContainment: "process",
	}, rels...)
}

func newVariantWorld(t *testing.T) *variantWorld {
	t.Helper()
	w := &variantWorld{t: t, dataDir: filepath.Join(t.TempDir(), "data"), binName: "llama-server"}
	if runtime.GOOS == "windows" {
		w.binName += ".exe"
	}
	cpu := tarGz(t, w.binName, []byte("cpu build"))
	cuda := tarGz(t, w.binName, []byte("cuda build"))
	libs := tarGz(t, "cudart64_13.dll", []byte("cuda runtime"))
	w.libs.Store(&libs)

	mux := http.NewServeMux()
	mux.HandleFunc("/cpu.tar.gz", func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(cpu) })
	mux.HandleFunc("/cuda.tar.gz", func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(cuda) })
	mux.HandleFunc("/cudart.tar.gz", func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(*w.libs.Load()) })
	vulkan := tarGz(t, w.binName, []byte("vulkan build"))
	mux.HandleFunc("/vulkan.tar.gz", func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(vulkan) })
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	base := localmodel.Release{Runtime: localmodel.Runtime, Version: "b99999", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, BinName: w.binName}
	cpuRel, cudaRel := base, base
	cpuRel.URL, cpuRel.SHA256, cpuRel.SizeBytes = srv.URL+"/cpu.tar.gz", sum(cpu), int64(len(cpu))
	cudaRel.URL, cudaRel.SHA256, cudaRel.SizeBytes = srv.URL+"/cuda.tar.gz", sum(cuda), int64(len(cuda))
	cudaRel.Variant, cudaRel.CUDA = "cuda-13.3", "13.3"
	cudaRel.Companions = []localmodel.Companion{{URL: srv.URL + "/cudart.tar.gz", SHA256: sum(libs), SizeBytes: int64(len(libs))}}

	vulkanRel := base
	vulkanRel.URL, vulkanRel.SHA256, vulkanRel.SizeBytes = srv.URL+"/vulkan.tar.gz", sum(vulkan), int64(len(vulkan))
	vulkanRel.Variant = "vulkan"

	w.cpuRel, w.cudaRel, w.vulkanRel = cpuRel, cudaRel, vulkanRel
	w.cfg = localmodel.WithReleases(localmodel.Config{
		DataDir: w.dataDir, HTTPClient: srv.Client(), MinContainment: "process",
	}, cpuRel, cudaRel)
	return w
}

// open returns a Host over the world's data directory that sees the machine as hw. Opening
// again gives a fresh Host over the same directory, as a later command would have.
func (w *variantWorld) open(hw localmodel.GPU) *localmodel.Host {
	w.t.Helper()
	h, err := localmodel.Open(w.cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	h.SetGPU(hw)
	w.t.Cleanup(func() { _ = h.Close() })
	return h
}

// The status a command reports must be the one the install produced: both resolve the
// runtime through the same selection and the same directory.
func TestRuntimeStatusReportsWhatInstallRuntimeInstalled(t *testing.T) {
	w := newVariantWorld(t)

	before, err := w.open(localmodel.GPU{}).RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before.Installed {
		t.Fatalf("an empty data directory reported an installed runtime: %+v", before)
	}

	inst, err := w.open(localmodel.GPU{}).InstallRuntime(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	after, err := w.open(localmodel.GPU{}).RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !after.Installed || after.BinPath != inst.BinPath || after.Variant != "cpu" || after.Problem != "" {
		t.Fatalf("status after install = %+v, want the build at %s", after, inst.BinPath)
	}
	if after.UsesGPU != metal {
		t.Errorf("UsesGPU = %v for a baseline build with no adapter found, want %v", after.UsesGPU, metal)
	}
}

func TestInstallRuntimeFetchesCUDAForAnNVIDIAGPU(t *testing.T) {
	w := newVariantWorld(t)

	inst, err := w.open(rtx).InstallRuntime(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Variant != "cuda-13.3" || inst.Cached {
		t.Fatalf("install = %+v, want a fresh cuda-13.3 build", inst)
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(inst.BinPath), "cudart64_13.dll")); err != nil || string(b) != "cuda runtime" {
		t.Fatalf("the CUDA runtime libraries are not beside the server: %q %v", b, err)
	}

	st, err := w.open(rtx).RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || st.Variant != "cuda-13.3" || !st.UsesGPU || st.GPU != rtx.Name || st.BinPath != inst.BinPath {
		t.Fatalf("status = %+v, want the installed CUDA build using %s", st, rtx.Name)
	}
	if got := strings.Join(st.Variants, ","); got != "cpu,cuda-13.3" {
		t.Errorf("Variants = %s, want cpu,cuda-13.3", got)
	}
}

// The variant an install chose is the one serving uses afterwards, with no GPU probe
// needed: a later command must not switch builds because detection came out differently.
func TestInstalledVariantIsTheOneEnsureRuntimeUses(t *testing.T) {
	w := newVariantWorld(t)
	inst, err := w.open(rtx).InstallRuntime(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}

	got, err := w.open(localmodel.GPU{}).EnsureRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.Variant != "cuda-13.3" || got.BinPath != inst.BinPath || !got.Cached {
		t.Fatalf("EnsureRuntime = %+v, want the cached CUDA build at %s", got, inst.BinPath)
	}
}

func TestInstallRuntimeHonoursAnExplicitVariant(t *testing.T) {
	w := newVariantWorld(t)
	h := w.open(rtx)

	inst, err := h.InstallRuntime(t.Context(), "cpu")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Variant != "cpu" {
		t.Fatalf("asked for cpu, got %s", inst.Variant)
	}
	st, err := w.open(rtx).RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Variant != "cpu" || st.UsesGPU != metal {
		t.Fatalf("status = %+v, want the baseline build reported as not using the NVIDIA GPU", st)
	}

	_, err = h.InstallRuntime(t.Context(), "cuda-9.9")
	if err == nil || !strings.Contains(err.Error(), "cpu, cuda-13.3") {
		t.Fatalf("an unknown variant must be refused with the valid names, got %v", err)
	}
}

// A runtime installed before variants existed sits where the CPU build still installs, with
// no record of a choice. It keeps being used on a machine that now has a GPU.
func TestCPURuntimeInstalledBeforeVariantsIsKept(t *testing.T) {
	w := newVariantWorld(t)
	inst, err := w.open(localmodel.GPU{}).InstallRuntime(t.Context(), "cpu")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(w.dataDir, "runtimes", localmodel.Runtime, "active")); err != nil {
		t.Fatal(err)
	}

	st, err := w.open(rtx).RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || st.Variant != "cpu" || st.BinPath != inst.BinPath {
		t.Fatalf("status = %+v, want the existing CPU build at %s", st, inst.BinPath)
	}
}

// A CUDA archive that does not match its pinned digest is refused, whichever of its two
// archives was altered, and nothing is left that could be run.
func TestTamperedCUDARuntimeLibrariesAreRefused(t *testing.T) {
	w := newVariantWorld(t)
	evil := tarGz(t, "cudart64_13.dll", []byte("tampered runtime"))
	w.libs.Store(&evil)

	h := w.open(rtx)
	if _, err := h.InstallRuntime(t.Context(), ""); err == nil {
		t.Fatal("a CUDA archive that does not match its pinned digest was installed")
	}
	st, err := w.open(rtx).RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Installed {
		t.Fatalf("a refused install left a build behind: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(w.dataDir, "runtimes", localmodel.Runtime, "active")); !os.IsNotExist(err) {
		t.Fatalf("a refused install recorded a choice (stat err=%v)", err)
	}
}

// A changed server binary is reported as a problem rather than as a healthy install.
func TestRuntimeStatusReportsAChangedBinary(t *testing.T) {
	w := newVariantWorld(t)
	inst, err := w.open(localmodel.GPU{}).InstallRuntime(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.BinPath, []byte("replaced"), 0o700); err != nil {
		t.Fatal(err)
	}

	st, err := w.open(localmodel.GPU{}).RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || st.Problem == "" {
		t.Fatalf("status = %+v, want an installed build with a reported problem", st)
	}
}

// A Vulkan build serves an AMD or Intel adapter, and is reported as idle on a machine with
// no adapter it could use.
func TestVulkanBuildIsReportedAsUsingTheAdapterItServes(t *testing.T) {
	w := newVariantWorld(t)
	w.cfg = w.config(w.cpuRel, w.vulkanRel)
	radeon := localmodel.GPU{Vulkan: "AMD Radeon RX 7900 XTX"}

	inst, err := w.open(radeon).InstallRuntime(t.Context(), "vulkan")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Variant != "vulkan" {
		t.Fatalf("asked for vulkan, got %s", inst.Variant)
	}
	st, err := w.open(radeon).RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.UsesGPU || st.GPU != radeon.Vulkan {
		t.Fatalf("status = %+v, want the Vulkan build using %s", st, radeon.Vulkan)
	}

	st, err = w.open(localmodel.GPU{}).RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.UsesGPU || st.GPU != "" {
		t.Fatalf("status = %+v, want a Vulkan build with no adapter reported as idle", st)
	}
}

// With no CPU baseline pinned and no GPU the machine can use, there is nothing to install,
// and install and status both say so instead of choosing a build that cannot load.
func TestNoBuildThatSuitsTheMachineIsAnError(t *testing.T) {
	w := newVariantWorld(t)
	w.cfg = w.config(w.cudaRel, w.vulkanRel)

	h := w.open(localmodel.GPU{})
	if _, err := h.InstallRuntime(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "suits this machine") {
		t.Fatalf("InstallRuntime = %v, want a refusal that no build suits this machine", err)
	}
	if _, err := h.RuntimeStatus(t.Context()); err == nil || !strings.Contains(err.Error(), "suits this machine") {
		t.Fatalf("RuntimeStatus = %v, want the same refusal", err)
	}
}

// A platform with no pinned build is reported by every entry point, before anything is
// fetched.
func TestPlatformWithoutAPinnedBuildIsRefused(t *testing.T) {
	w := newVariantWorld(t)
	elsewhere := w.cpuRel
	elsewhere.GOOS, elsewhere.GOARCH = "plan9", "mips"
	w.cfg = w.config(elsewhere)

	h := w.open(localmodel.GPU{})
	if _, err := h.InstallRuntime(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "no pinned") {
		t.Errorf("InstallRuntime = %v, want a refusal naming the missing pin", err)
	}
	if _, err := h.EnsureRuntime(t.Context()); err == nil {
		t.Error("EnsureRuntime found a build for a platform with none pinned")
	}
	if _, err := h.RuntimeStatus(t.Context()); err == nil {
		t.Error("RuntimeStatus found a build for a platform with none pinned")
	}
}

// An install whose choice cannot be recorded fails rather than reporting a build later
// commands would not use.
func TestInstallRuntimeFailsWhenTheChoiceCannotBeRecorded(t *testing.T) {
	w := newVariantWorld(t)
	if err := os.MkdirAll(filepath.Join(w.dataDir, "runtimes", localmodel.Runtime, "active"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := w.open(localmodel.GPU{}).InstallRuntime(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "record the chosen runtime build") {
		t.Fatalf("InstallRuntime = %v, want a failure to record the choice", err)
	}
}

func TestACompanionWithoutADigestIsRefusedAtOpen(t *testing.T) {
	w := newVariantWorld(t)
	rel := w.cudaRel
	rel.Companions = []localmodel.Companion{{URL: w.cudaRel.Companions[0].URL}}

	if _, err := localmodel.Open(w.config(w.cpuRel, rel)); !errors.Is(err, localmodel.ErrUnpinned) {
		t.Fatalf("Open with an unpinned companion = %v, want ErrUnpinned", err)
	}
}

// With no releases supplied a Host uses the pins in this module and probes the machine
// itself, which is what the shipped command does.
func TestRuntimeStatusUsesTheModulePinsAndProbesTheMachine(t *testing.T) {
	h, err := localmodel.Open(localmodel.Config{DataDir: filepath.Join(t.TempDir(), "data"), MinContainment: "process"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })

	st, err := h.RuntimeStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Installed || st.Version == "" || len(st.Variants) == 0 || st.Variants[0] != "cpu" {
		t.Fatalf("status = %+v, want an absent runtime with the CPU baseline pinned first", st)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 MiB"},
		{1 << 20, "1 MiB"},
		{525 << 20, "525 MiB"},
		{1 << 30, "1.0 GiB"},
		{3 << 29, "1.5 GiB"},
	}
	for _, tc := range tests {
		if got := localmodel.FormatBytes(tc.n); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
