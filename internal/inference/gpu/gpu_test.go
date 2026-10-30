package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/inference"
)

// scripted answers a command from a table keyed by the full command line; an absent line
// fails the way a missing tool does.
func scripted(replies map[string]string) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		line := strings.TrimSpace(name + " " + strings.Join(args, " "))
		out, ok := replies[line]
		if !ok {
			return nil, errors.New("exec: not found or unsupported: " + line)
		}
		return []byte(out), nil
	}
}

const (
	fullQuery = "nvidia-smi --query-gpu=name,driver_version,compute_cap --format=csv,noheader"
	oldQuery  = "nvidia-smi --query-gpu=name,driver_version --format=csv,noheader"
)

func TestDetectReadsNameDriverCUDAAndCompute(t *testing.T) {
	run := scripted(map[string]string{
		fullQuery:    "NVIDIA GeForce RTX 4070 Ti, 610.88, 8.9\r\n",
		"nvidia-smi": "| NVIDIA-SMI 610.88                 KMD Version: 610.88        CUDA UMD Version: 13.3     |\n",
	})
	got := Detect(context.Background(), run)
	if !got.NVIDIA() || got.Name != "NVIDIA GeForce RTX 4070 Ti" || got.Driver != "610.88" {
		t.Fatalf("adapter not read: %+v", got)
	}
	if got.CUDA.String() != "13.3" {
		t.Errorf("CUDA = %v, want 13.3", got.CUDA)
	}
	if got.ComputeCap != 8.9 {
		t.Errorf("ComputeCap = %v, want 8.9", got.ComputeCap)
	}
}

func TestDetectFallsBackForADriverWithoutComputeCap(t *testing.T) {
	run := scripted(map[string]string{
		oldQuery:     "Quadro P2000, 472.12\n",
		"nvidia-smi": "| NVIDIA-SMI 472.12       Driver Version: 472.12       CUDA Version: 11.4     |\n",
	})
	got := Detect(context.Background(), run)
	if got.Name != "Quadro P2000" || got.ComputeCap != 0 || got.CUDA.String() != "11.4" {
		t.Fatalf("old driver not read: %+v", got)
	}
}

func TestDetectReportsNoGPU(t *testing.T) {
	tests := []struct {
		name    string
		replies map[string]string
	}{
		{"no tool", map[string]string{}},
		{"tool prints nothing", map[string]string{fullQuery: "\n"}},
		{"tool prints an error banner", map[string]string{fullQuery: "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver."}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Detect(context.Background(), scripted(tc.replies)); got.NVIDIA() {
				t.Fatalf("found a GPU where there is none: %+v", got)
			}
		})
	}
}

func TestDetectKeepsAnAdapterWhenTheTableIsUnreadable(t *testing.T) {
	run := scripted(map[string]string{fullQuery: "GeForce GTX 1080, 551.23, 6.1\n"})
	got := Detect(context.Background(), run)
	if !got.NVIDIA() || got.CUDA != nil {
		t.Fatalf("want an adapter with an unknown CUDA version, got %+v", got)
	}
}

func TestDetectFindsAVulkanAdapterWhenThereIsNoNVIDIA(t *testing.T) {
	const ps = "powershell -NoProfile -NonInteractive -Command (Get-CimInstance Win32_VideoController).Name"
	tests := []struct {
		name    string
		goos    string
		replies map[string]string
		want    string
	}{
		{"windows radeon", "windows", map[string]string{ps: "AMD Radeon RX 7800 XT\r\n"}, "AMD Radeon RX 7800 XT"},
		{"windows arc", "windows", map[string]string{ps: "Intel(R) Arc(TM) A770 Graphics\r\n"}, "Intel(R) Arc(TM) A770 Graphics"},
		{"windows integrated only", "windows", map[string]string{ps: "Intel(R) UHD Graphics 770\r\n"}, ""},
		{"linux radeon", "linux", map[string]string{"lspci": "03:00.0 VGA compatible controller: AMD Radeon RX 6700 XT\n"}, "AMD Radeon RX 6700 XT"},
		{"linux no lspci", "linux", map[string]string{}, ""},
		{"macos is not asked", "darwin", map[string]string{ps: "AMD Radeon Pro 5500M\n"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detect(context.Background(), scripted(tc.replies), tc.goos)
			if got.NVIDIA() || got.Vulkan != tc.want {
				t.Fatalf("detect = %+v, want Vulkan %q and no NVIDIA", got, tc.want)
			}
		})
	}
}

func TestDetectPrefersNVIDIAOverOtherAdapters(t *testing.T) {
	run := scripted(map[string]string{
		fullQuery: "NVIDIA GeForce RTX 4070 Ti, 610.88, 8.9\n",
		"lspci":   "03:00.0 VGA compatible controller: AMD Radeon RX 6700 XT\n",
	})
	if got := detect(context.Background(), run, "linux"); !got.NVIDIA() || got.Vulkan != "" {
		t.Fatalf("an NVIDIA card must win: %+v", got)
	}
}

func TestParseCUDAVersion(t *testing.T) {
	tests := []struct {
		in   string
		want inference.Version
	}{
		{"CUDA Version: 12.4", inference.Version{12, 4}},
		{"CUDA UMD Version: 13.3", inference.Version{13, 3}},
		{"CUDA Version:   12.10", inference.Version{12, 10}},
		{"no version here", nil},
		{"", nil},
	}
	for _, tc := range tests {
		got := parseCUDAVersion(tc.in)
		if got.String() != tc.want.String() {
			t.Errorf("parseCUDAVersion(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestExecRunnerReportsAMissingTool(t *testing.T) {
	if _, err := ExecRunner(context.Background(), "flynn-no-such-gpu-tool"); err == nil {
		t.Fatal("a missing tool must be an error")
	}
	if got := Detect(context.Background(), nil); got.NVIDIA() && got.Name == "" {
		t.Fatalf("inconsistent info from the default runner: %+v", got)
	}
}
