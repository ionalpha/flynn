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
}

// HasGPU reports whether a GPU with known memory was detected.
func (b Box) HasGPU() bool { return b.VRAMBytes > 0 }

// HasRAM reports whether the system memory total was read.
func (b Box) HasRAM() bool { return b.RAMBytes > 0 }

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
		}
	}
	b.RAMBytes = systemRAMBytes(ctx)
	return b
}

// runNvidiaSMI queries the NVIDIA management tool for total memory and name, returning
// false when the tool is missing or errors (no GPU, no driver, not installed).
func runNvidiaSMI(ctx context.Context) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=memory.total,name", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// parseNvidiaSMI reads the first GPU row of the query output ("<MiB>, <name>") into a
// Box. Memory is reported in mebibytes with the nounits format, so it is scaled to
// bytes. It returns false when no row parses.
func parseNvidiaSMI(out string) (Box, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		field, name, ok := strings.Cut(line, ",")
		if !ok {
			continue
		}
		mib, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || mib <= 0 {
			continue
		}
		return Box{GPUName: strings.TrimSpace(name), VRAMBytes: mib * 1024 * 1024}, true
	}
	return Box{}, false
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
