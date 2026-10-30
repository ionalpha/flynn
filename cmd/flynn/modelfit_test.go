package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/hardware"
)

// TestRenderFitDefaultsUnsetSettings: a caller that passes no settings gets the catalog's
// default context and an f16 cache, named in the heading, rather than a zero-token fit.
func TestRenderFitDefaultsUnsetSettings(t *testing.T) {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	mc := fitMachine{Machine: catalog.Machine{VRAMFreeBytes: 16_000_000_000, RAMBytes: 16_000_000_000}, description: "a 16 GB card"}
	var b strings.Builder
	renderFit(&b, cat, cat.Find(catalog.Query{Kind: catalog.KindLocal}), mc, catalog.FitSettings{}, localProfileSource(context.Background(), ":memory:"))
	if want := "context " + contextCol(catalog.DefaultContextTokens) + ", KV cache f16"; !strings.Contains(b.String(), want) {
		t.Fatalf("heading missing %q:\n%s", want, b.String())
	}
}

// TestPlacementColNamesEachPlacement covers the label for a plan that has no placement,
// which is how a model with nothing known about its size is shown.
func TestPlacementColNamesEachPlacement(t *testing.T) {
	for _, tc := range []struct {
		plan catalog.FitPlan
		want string
	}{
		{catalog.FitPlan{Placement: catalog.PlacementGPU}, "gpu"},
		{catalog.FitPlan{Placement: catalog.PlacementCPU}, "cpu"},
		{catalog.FitPlan{Placement: catalog.PlacementSplit, ExpertsOnCPU: 0.25, RelativeSpeed: 0.6}, "gpu+ram (25% of experts on CPU, ~60% speed)"},
		{catalog.FitPlan{}, "-"},
	} {
		if got := placementCol(tc.plan); got != tc.want {
			t.Errorf("placementCol(%+v) = %q, want %q", tc.plan, got, tc.want)
		}
	}
}

// stubMachine makes the fit view see a described box instead of the one running the test.
// freeBytes < 0 means the free-memory probe is unavailable.
func stubMachine(t *testing.T, box hardware.Box, freeBytes int64) {
	t.Helper()
	oldBox, oldFree := detectBox, probeFreeVRAM
	t.Cleanup(func() { detectBox, probeFreeVRAM = oldBox, oldFree })
	detectBox = func(context.Context) hardware.Box { return box }
	probeFreeVRAM = func(context.Context) (int64, bool) { return freeBytes, freeBytes >= 0 }
}

func fitOutput(t *testing.T, args ...string) string {
	t.Helper()
	var b strings.Builder
	if err := runModels(append([]string{"--local", "--fit"}, args...), "", &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestFitViewOnTheFieldTestBox is the laptop the change was made for: a 16 GB card with
// 1.1 GB held by the desktop and 16 GB of RAM. The 35B mixture-of-experts model tops the
// list as a labelled split, and the largest model that stays on the card is named beside it.
func TestFitViewOnTheFieldTestBox(t *testing.T) {
	const gb = 1 << 30
	stubMachine(t, hardware.Box{GPUName: "RTX 5080 Laptop", VRAMBytes: 16 * gb, RAMBytes: 16 * 1_000_000_000}, 16*gb-1_100_000_000)
	out := fitOutput(t)
	for _, want := range []string{
		"RTX 5080 Laptop", "in use", "context 32k, KV cache f16",
		"ON GPU", "IN RAM", "KV CACHE",
		"recommended local model: qwen3.6:35b-a3b (gpu+ram",
		"largest that stays wholly on the GPU: gpt-oss-20b",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("fit view missing %q:\n%s", want, out)
		}
	}
}

// TestFitViewTable runs the view over the machines a user is likely to have.
func TestFitViewTable(t *testing.T) {
	const gb = 1_000_000_000
	cases := []struct {
		name  string
		box   hardware.Box
		free  int64
		args  []string
		want  string
		wants []string
	}{
		{
			name: "12GB card, 64GB RAM",
			box:  hardware.Box{GPUName: "RTX 4070", VRAMBytes: 12 * gb, RAMBytes: 64 * gb},
			free: 11 * gb,
			want: "recommended local model: qwen3.6:35b-a3b (gpu+ram",
		},
		{
			name: "CPU only",
			box:  hardware.Box{RAMBytes: 16 * gb},
			free: -1,
			want: "recommended local model: ",
			wants: []string{
				"system RAM", "usable",
			},
		},
		{
			name: "24GB card holds the 35B whole",
			box:  hardware.Box{GPUName: "RTX 4090", VRAMBytes: 24 * gb, RAMBytes: 32 * gb},
			free: 23 * gb,
			want: "recommended local model: qwen3.6:35b-a3b (gpu)",
		},
		{
			name:  "a q8_0 cache at 131k tokens leaves the 35B tight where f16 leaves it over budget",
			box:   hardware.Box{GPUName: "RTX 4090", VRAMBytes: 24 * gb},
			free:  23 * gb,
			args:  []string{"--ctx", "131072", "--kv", "q8"},
			want:  "KV cache q8_0",
			wants: []string{"tight"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubMachine(t, tc.box, tc.free)
			out := fitOutput(t, tc.args...)
			for _, want := range append([]string{tc.want}, tc.wants...) {
				if !strings.Contains(out, want) {
					t.Fatalf("fit view missing %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestFitViewRejectsBadSettings(t *testing.T) {
	stubMachine(t, hardware.Box{RAMBytes: 16_000_000_000}, -1)
	for _, args := range [][]string{{"--kv", "f32"}, {"--ctx", "0"}, {"--ctx", "-5"}} {
		var b strings.Builder
		if err := runModels(append([]string{"--local", "--fit"}, args...), "", &b); err == nil {
			t.Fatalf("%v accepted:\n%s", args, b.String())
		}
	}
}

// TestFitViewWithNothingDetected says so rather than ranking against zero memory.
func TestFitViewWithNothingDetected(t *testing.T) {
	stubMachine(t, hardware.Box{}, -1)
	out := fitOutput(t)
	if !strings.Contains(out, "could not detect") {
		t.Fatalf("expected the undetected message:\n%s", out)
	}
}

// TestGivenVRAMIgnoresThisMachinesRAM keeps `--vram` reproducible: the same flags give the
// same answer on a laptop with 16 GB of RAM and a workstation with 256 GB, and --ram opts
// into a split.
func TestGivenVRAMIgnoresThisMachinesRAM(t *testing.T) {
	stubMachine(t, hardware.Box{GPUName: "big", VRAMBytes: 80_000_000_000, RAMBytes: 256_000_000_000}, -1)
	without := fitOutput(t, "--vram", "16")
	if strings.Contains(without, "gpu+ram (") || !strings.Contains(without, "no system RAM") {
		t.Fatalf("--vram alone must not borrow this machine's RAM:\n%s", without)
	}
	with := fitOutput(t, "--vram", "16", "--ram", "16")
	if !strings.Contains(with, "recommended local model: qwen3.6:35b-a3b (gpu+ram") {
		t.Fatalf("--ram should enable the split:\n%s", with)
	}
}

// TestFreeVRAMProbeAbsentFallsBackToTotal: with no probe the detected total is used and
// the line does not claim anything is in use.
func TestFreeVRAMProbeAbsentFallsBackToTotal(t *testing.T) {
	stubMachine(t, hardware.Box{GPUName: "X", VRAMBytes: 16_000_000_000, RAMBytes: 8_000_000_000}, -1)
	m := resolveMachine(0, 0)
	if m.VRAMFreeBytes != 16_000_000_000 || strings.Contains(m.description, "in use") {
		t.Fatalf("machine = %+v", m)
	}
}
