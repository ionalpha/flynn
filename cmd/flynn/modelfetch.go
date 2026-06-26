package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/fetch"
	"github.com/ionalpha/flynn/inference/launch"
)

// runModelFetch implements `flynn models fetch <id>`: download a catalog model's
// weights and verify them, then record them as available. It NEVER runs the model.
// A model file is untrusted input to a known-vulnerable parser, so running it is a
// separate step that happens only inside the isolation sandbox; fetch stops at a
// verified file on disk.
func runModelFetch(args []string, dataDir string, out io.Writer) error {
	fs := flag.NewFlagSet("models fetch", flag.ContinueOnError)
	fs.SetOutput(out)
	quantName := fs.String("quant", "", "which quantization to fetch (default: the smallest)")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(out, "usage: flynn models fetch <model-id> [--quant <name>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	id := fs.Arg(0)
	if id == "" {
		fs.Usage()
		return errors.New("models fetch: a model id is required")
	}

	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	m, ok := findModel(cat, id)
	if !ok {
		return fmt.Errorf("models fetch: %q is not in the catalog (see `flynn models`)", id)
	}
	if !m.Local() {
		return fmt.Errorf("models fetch: %q is a hosted API model; there is nothing to download (select it with --model %s)", id, id)
	}
	q, ok := pickQuant(m, *quantName)
	if !ok {
		return fmt.Errorf("models fetch: %q has no quantization %q", id, *quantName)
	}
	// Model policy (not the generic downloader's concern): never fetch weights in a
	// format that executes code when a runtime loads them.
	if isCodeExecWeight(q.Format) {
		return fmt.Errorf("models fetch: %q quant %q uses a code-executing weight format and will not be fetched", id, q.Name)
	}
	if q.URL == "" {
		_, _ = fmt.Fprintf(out, "%s (%s) has no pinned download URL yet, so it cannot be fetched and verified.\nThe catalog records this model but a direct, digest-pinned source has not been added.\n", id, q.Name)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dest := filepath.Join(dataDir, "models", weightsFileName(id, q))
	_, _ = fmt.Fprintf(out, "fetching %s (%s, %s) from %s\n", id, q.Name, humanBytes(q.SizeBytes), q.URL)
	res, err := fetch.New().Fetch(ctx, fetch.Request{
		URL:          q.URL,
		Dest:         dest,
		ExpectSHA256: q.Digest,
		MaxBytes:     sizeCeiling(q.SizeBytes),
	})
	if err != nil {
		return fmt.Errorf("models fetch: %w", err)
	}

	trust := "pinned to the catalog digest"
	if !res.Pinned {
		trust = "pinned on first fetch (the catalog entry had no digest, lower trust)"
	}
	_, _ = fmt.Fprintf(out, "fetched and verified: %s\n  %s, sha256:%s, %s\n", res.Path, humanBytes(res.Bytes), res.SHA256[:12], trust)

	// Read the weights with the hardened GGUF reader, never the runtime's parser, and
	// settle the chat template before the model is ever run. This catches a file our own
	// reader cannot parse (so the runtime's CVE-prone parser is never handed something
	// malformed) and surfaces whether the model ships its own template, which is
	// overridden by the trusted one at serve time regardless.
	_, _ = fmt.Fprintln(out, inspectWeights(res.Path, m.ChatTemplate))

	_, _ = fmt.Fprintln(out, "not started: a downloaded model is run only inside the isolation sandbox. Start it with `flynn models run "+id+"`.")
	return nil
}

// inspectWeights parses the fetched weights with the hardened GGUF reader and returns a
// human line describing the chat-template decision. It never returns an error: a parse
// failure is reported as a caution (the file stays on disk, already digest-verified)
// rather than aborting, because the security value is exactly that the hardened reader,
// not the runtime, is what first reads the untrusted file. An empty trusted template is
// reported, since a local model must carry one to be served.
func inspectWeights(path, trusted string) string {
	if trusted == "" {
		return "  template: this model has no trusted chat template in the catalog; it cannot be served until one is set"
	}
	decision, err := launch.InspectTemplate(path, trusted)
	if err != nil {
		return "  template: caution: the hardened GGUF reader could not parse these weights (" + err.Error() + "); the runtime will not be handed this file until it can be read safely"
	}
	if decision.ModelSupplied {
		return "  template: the model ships its own chat template; it will be overridden with the trusted \"" + trusted + "\" template at serve time"
	}
	return "  template: will be served with the trusted \"" + trusted + "\" template"
}

// findModel looks up a catalog entry by its exact id.
func findModel(cat catalog.Catalog, id string) (catalog.ModelSpec, bool) {
	for _, m := range cat.Models {
		if m.ID == id {
			return m, true
		}
	}
	return catalog.ModelSpec{}, false
}

// pickQuant returns the named quantization, or the smallest when no name is given.
func pickQuant(m catalog.ModelSpec, name string) (catalog.Quant, bool) {
	if name != "" {
		for _, q := range m.Quants {
			if strings.EqualFold(q.Name, name) {
				return q, true
			}
		}
		return catalog.Quant{}, false
	}
	return m.SmallestQuant()
}

// isCodeExecWeight reports whether a weight format executes code when a runtime
// loads it (pickle), which the model layer refuses to fetch even though the generic
// downloader is content-agnostic.
func isCodeExecWeight(f catalog.Format) bool {
	return f == catalog.FormatPickle
}

// sizeCeiling caps a download at the quantization's declared size plus a small
// margin, so a server cannot stream more than the model is supposed to be. An
// unknown size (0) leaves the downloader's own default ceiling in place.
func sizeCeiling(size int64) int64 {
	if size <= 0 {
		return 0
	}
	return size + size/20 // +5%
}

// weightsFileName builds a filesystem-safe name for the installed weights from the
// model id and quantization, so two models never collide on disk.
func weightsFileName(id string, q catalog.Quant) string {
	safe := strings.NewReplacer("/", "_", ":", "_", "\\", "_", " ", "_").Replace(id)
	return safe + "-" + strings.NewReplacer("/", "_", " ", "_").Replace(q.Name) + ".gguf"
}
