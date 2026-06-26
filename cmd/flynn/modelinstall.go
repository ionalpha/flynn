package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/inference/provision"
)

// runRuntimeInstall implements `flynn models install [runtime]`: fetch a pinned, gated
// local inference runtime so a machine with none can run a model without any manual
// setup. The default runtime is the one Flynn provisions itself, llama.cpp; its release
// is small and ships for every common platform. The build is verified against a pinned
// digest by the download path and gated on its version before it is offered, so an
// install only ever places a known, safe runtime on disk. It does not start anything.
func runRuntimeInstall(args []string, dataDir string, out io.Writer) error {
	name := "llama.cpp"
	if len(args) >= 1 && args[0] != "" {
		name = args[0]
	}

	rel, ok := provision.ReleaseFor(name, runtime.GOOS, runtime.GOARCH)
	if !ok {
		return fmt.Errorf("models install: no pinned %s build for %s/%s; Flynn can provision: %s",
			name, runtime.GOOS, runtime.GOARCH, strings.Join(installableRuntimes(), ", "))
	}
	if err := rel.Gate(); err != nil {
		return fmt.Errorf("models install: refusing to install %s %s: %w", name, rel.Version, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dest := filepath.Join(dataDir, "runtimes")
	_, _ = fmt.Fprintf(out, "installing %s %s for %s/%s (%s, verified against a pinned digest)\n",
		name, rel.Version, rel.GOOS, rel.GOARCH, humanBytes(rel.SizeBytes))
	res, err := provision.Install(ctx, fetch.New(), rel, dest)
	if err != nil {
		return fmt.Errorf("models install: %w", err)
	}
	if res.FromCache {
		_, _ = fmt.Fprintf(out, "already installed: %s\n", res.BinPath)
		return nil
	}
	_, _ = fmt.Fprintf(out, "installed and verified: %s\n", res.BinPath)
	_, _ = fmt.Fprintln(out, "the runtime is on disk, gate-approved, and not started: a model runs only inside the isolation sandbox.")
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
