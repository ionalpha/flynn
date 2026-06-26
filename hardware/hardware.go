// Package hardware probes the local machine for the resources that decide which
// models it can run, today the GPU and its memory. It is a best-effort diagnostic,
// not part of a governed run: detection shells out to a vendor tool when present and
// reports nothing rather than guessing when it is absent, so a caller can fall back to
// an explicit budget. The parsing is pure and tested; only the probe itself touches
// the machine.
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
}

// HasGPU reports whether a GPU with known memory was detected.
func (b Box) HasGPU() bool { return b.VRAMBytes > 0 }

// Detect probes the machine. It is best-effort: an absent or failing probe yields a
// zero Box, never an error, so callers degrade to an explicit budget instead of
// failing. The context bounds the probe so a wedged tool cannot hang the caller.
func Detect(ctx context.Context) Box {
	if out, ok := runNvidiaSMI(ctx); ok {
		if b, ok := parseNvidiaSMI(out); ok {
			return b
		}
	}
	return Box{}
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
