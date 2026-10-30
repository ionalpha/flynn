package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	gruntime "runtime"
	"strings"

	"github.com/ionalpha/flynn/internal/inference"
	"github.com/ionalpha/flynn/internal/inference/provision"
	"github.com/ionalpha/flynn/localmodel"
	"github.com/ionalpha/flynn/sandbox"
)

// runRuntimeCheck reports, for each local inference runtime Flynn knows, whether it is
// installed and whether its version is exposed to a known model-parser advisory. It is
// the security gate made visible: the same check the run path uses before launching a
// runtime, surfaced as a command. A vulnerable runtime is reported with the advisory
// so it can be updated; an absent one is simply not installed.
func runRuntimeCheck(out io.Writer) error { return runRuntimeCheckAt(defaultDataDir(), out) }

// runRuntimeCheckAt is runRuntimeCheck over the data directory `flynn models install`
// wrote to. Without it the check could only look for a binary on PATH, and a runtime
// Flynn had just installed would be reported as missing.
func runRuntimeCheckAt(dataDir string, out io.Writer) error {
	ctx := context.Background()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	sb, err := sandbox.NewLocal(cwd)
	if err != nil {
		return err
	}
	defer func() { _ = sb.Close() }()

	// A data directory that cannot be opened leaves the PATH probe as the only source, which
	// is what the report said before Flynn installed anything itself.
	var installed ownRuntimeStatus
	if host, err := openHost(dataDir, io.Discard); err == nil {
		defer func() { _ = host.Close() }()
		installed = host.RuntimeStatus
	}
	return reportRuntimesWith(ctx, sb, installed, out)
}

// ownRuntimeStatus reports the build of the runtime Flynn provisions itself, as
// [localmodel.Host.RuntimeStatus] does: the same lookup the install and the run path use.
type ownRuntimeStatus func(ctx context.Context) (localmodel.RuntimeStatus, error)

// reportRuntimes writes the installed-and-safe report for every known runtime, probing
// each one through the given sandbox. It is runRuntimeCheck with the boundary supplied,
// so the report is exercised against scripted runtime versions.
func reportRuntimes(ctx context.Context, sb sandbox.Sandbox, out io.Writer) error {
	return reportRuntimesWith(ctx, sb, nil, out)
}

// reportRuntimesWith is reportRuntimes plus the build Flynn installed itself. That build is
// the one a run would use, so it is reported first, with its variant and whether it offloads
// to the GPU; a binary found on PATH is reported only for a runtime Flynn has not installed.
func reportRuntimesWith(ctx context.Context, sb sandbox.Sandbox, own ownRuntimeStatus, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "local inference runtimes:")
	for _, rt := range inference.Runtimes() {
		var st localmodel.RuntimeStatus
		if own != nil && rt.Name == localmodel.Runtime {
			st, _ = own(ctx)
		}
		if st.Installed {
			reportOwnRuntime(out, rt, st)
			continue
		}
		ver, ok := detectRuntimeVersion(ctx, sb, rt)
		switch {
		case !ok:
			if _, can := provision.ReleaseFor(rt.Name, gruntime.GOOS, gruntime.GOARCH); can {
				_, _ = fmt.Fprintf(out, "  %-10s not installed (run `flynn models install %s` to fetch a pinned build)\n", rt.Name, rt.Name)
				if st.Variant != "" {
					_, _ = fmt.Fprintf(out, "  %-10s that installs the %s build for this machine\n", "", st.Variant)
				}
			} else {
				_, _ = fmt.Fprintf(out, "  %-10s not installed\n", rt.Name)
			}
		case inference.SafeToRun(rt.Name, ver) != nil:
			_, _ = fmt.Fprintf(out, "  %-10s %-8s VULNERABLE: %s (update the runtime)\n", rt.Name, ver, concern(rt.Name, ver))
		default:
			_, _ = fmt.Fprintf(out, "  %-10s %-8s ok\n", rt.Name, ver)
		}
	}
	return nil
}

// reportOwnRuntime writes the lines for the build Flynn installed: its verdict, the variant,
// and whether the GPU is used. A CPU build on a machine with a GPU is called out, since it
// is the case where a model runs several times slower than it needs to.
func reportOwnRuntime(out io.Writer, rt inference.Runtime, st localmodel.RuntimeStatus) {
	ver := inference.ParseVersion(st.Version)
	switch {
	case st.Problem != "":
		_, _ = fmt.Fprintf(out, "  %-10s %-8s CORRUPT: %s (remove %s and run `flynn models install %s` again)\n",
			rt.Name, ver, st.Problem, filepath.Dir(st.BinPath), rt.Name)
	case inference.SafeToRun(rt.Name, ver) != nil:
		_, _ = fmt.Fprintf(out, "  %-10s %-8s VULNERABLE: %s (update the runtime)\n", rt.Name, ver, concern(rt.Name, ver))
	default:
		_, _ = fmt.Fprintf(out, "  %-10s %-8s ok\n", rt.Name, ver)
	}
	_, _ = fmt.Fprintf(out, "  %-10s variant: %s (installed by flynn)\n", "", st.Variant)
	switch {
	case st.UsesGPU:
		_, _ = fmt.Fprintf(out, "  %-10s GPU: %s, used by this build\n", "", st.GPU)
	case st.GPU != "":
		_, _ = fmt.Fprintf(out, "  %-10s GPU: %s, not used by this build (`flynn models install %s --variant <name>`: %s)\n",
			"", st.GPU, rt.Name, strings.Join(st.Variants, ", "))
	default:
		_, _ = fmt.Fprintf(out, "  %-10s GPU: none detected, running on the CPU\n", "")
	}
}

// detectRuntimeVersion finds an installed binary for the runtime and reads its version
// by running it through the sandbox boundary. It returns the parsed version and whether
// a working binary was found.
func detectRuntimeVersion(ctx context.Context, sb sandbox.Sandbox, rt inference.Runtime) (inference.Version, bool) {
	for _, bin := range rt.Binaries {
		line := bin + " " + strings.Join(rt.VersionArgs, " ")
		res, err := sb.Exec(ctx, sandbox.Command{Line: line})
		if err != nil || res.ExitCode != 0 {
			continue
		}
		ver := rt.ParseVersion(res.Output)
		if len(ver) == 0 {
			continue
		}
		return ver, true
	}
	return nil, false
}

// concern explains why a runtime version is unsafe, for the report: the named
// advisories it is exposed to, or, when it is below the floor with no named advisory,
// that it predates the minimum supported version.
func concern(runtime string, v inference.Version) string {
	ex := inference.Exposure(runtime, v, inference.Advisories())
	if len(ex) > 0 {
		ids := make([]string, len(ex))
		for i, a := range ex {
			ids[i] = a.ID
		}
		return strings.Join(ids, ", ")
	}
	if floor, ok := inference.MinSupportedFor(runtime); ok {
		return "older than minimum supported " + floor.String()
	}
	return "unsafe"
}
