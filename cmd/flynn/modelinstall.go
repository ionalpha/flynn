package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/internal/inference/provision"
	"github.com/ionalpha/flynn/localmodel"
)

// runRuntimeInstall implements `flynn models install [runtime] [--variant name]`: fetch a
// pinned, gated local inference runtime so a machine with none can run a model without any
// manual setup. The default runtime is the one Flynn provisions itself, llama.cpp, which
// ships for every common platform. On a machine with an NVIDIA GPU the build is the CUDA
// one, since the CPU build would leave the card idle; --variant picks a build by name
// instead. Every archive is verified against a pinned digest by the download path, and the
// version is gated before it is offered, so an install only ever places a known, safe
// runtime on disk. It does not start anything.
func runRuntimeInstall(args []string, dataDir string, out io.Writer) error {
	name := localmodel.Runtime
	if len(args) >= 1 && !strings.HasPrefix(args[0], "-") {
		if args[0] != "" {
			name = args[0]
		}
		args = args[1:]
	}
	fs := flag.NewFlagSet("models install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	variant := fs.String("variant", "", "build to install, such as cpu, cuda-13.3 or vulkan; the default fits this machine's GPU")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("models install: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("models install: unexpected argument %q (usage: flynn models install [runtime] [--variant name])", fs.Arg(0))
	}

	// Only the runtime the public API serves is installed here; it applies the advisory gate
	// before any download and checks the archive against its pinned digest.
	if _, ok := provision.ReleaseFor(name, runtime.GOOS, runtime.GOARCH); !ok || name != localmodel.Runtime {
		return fmt.Errorf("models install: no pinned %s build for %s/%s; Flynn can provision: %s",
			name, runtime.GOOS, runtime.GOARCH, strings.Join(installableRuntimes(), ", "))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	host, err := openHost(dataDir, out)
	if err != nil {
		return fmt.Errorf("models install: %w", err)
	}
	defer func() { _ = host.Close() }()
	return installRuntime(ctx, host, name, *variant, out)
}

// runtimeInstaller is what an install needs of a [localmodel.Host], so the messages it prints
// are exercised against scripted outcomes without downloading a runtime.
type runtimeInstaller interface {
	RuntimeStatus(ctx context.Context) (localmodel.RuntimeStatus, error)
	InstallRuntime(ctx context.Context, variant string) (localmodel.RuntimeInstall, error)
}

// installRuntime installs the named variant (empty for the build that fits the machine) and
// tells the user which build was chosen, and when it leaves a detected GPU idle.
func installRuntime(ctx context.Context, host runtimeInstaller, name, variant string, out io.Writer) error {
	_, _ = fmt.Fprintf(out, "installing %s for %s/%s (verified against a pinned digest)\n", name, runtime.GOOS, runtime.GOARCH)
	if st, err := host.RuntimeStatus(ctx); err == nil {
		if st.GPU != "" {
			_, _ = fmt.Fprintf(out, "detected GPU: %s\n", st.GPU)
		} else {
			_, _ = fmt.Fprintln(out, "no GPU detected")
		}
	}
	res, err := host.InstallRuntime(ctx, variant)
	if err != nil {
		return fmt.Errorf("models install: %w", err)
	}
	if res.Cached {
		_, _ = fmt.Fprintf(out, "already installed: %s (variant %s)\n", res.BinPath, res.Variant)
	} else {
		_, _ = fmt.Fprintf(out, "installed and verified: %s (variant %s)\n", res.BinPath, res.Variant)
		_, _ = fmt.Fprintln(out, "the runtime is on disk, gate-approved, and not started: a model runs only inside the isolation sandbox.")
	}
	if st, err := host.RuntimeStatus(ctx); err == nil && st.GPU != "" && !st.UsesGPU {
		_, _ = fmt.Fprintf(out, "note: the %s build runs on the CPU and leaves %s idle; `flynn models install %s --variant <name>` fetches another (%s)\n",
			st.Variant, st.GPU, name, strings.Join(st.Variants, ", "))
	}
	return nil
}

// installableRuntimes lists the distinct runtime names Flynn has a pinned build for on
// any platform, so a failed lookup can name what is available.
func installableRuntimes() []string {
	seen := map[string]bool{}
	var names []string
	for _, r := range provision.Releases() {
		if !seen[r.Runtime] {
			seen[r.Runtime] = true
			names = append(names, r.Runtime)
		}
	}
	return names
}
