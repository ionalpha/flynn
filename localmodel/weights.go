package localmodel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/gguf"
	"github.com/ionalpha/flynn/internal/inference/launch"
)

// Weights is a verified weights file on disk.
type Weights struct {
	// Path is the file. It holds exactly the bytes whose hash is SHA256.
	Path string
	// Quant names the quantization.
	Quant string
	// SHA256 is the hex digest the file was verified against.
	SHA256 string
}

// EnsureWeights makes a quantization of a catalog model available on disk and returns
// it. quant names the quantization; empty picks the smallest single-file GGUF. A
// quantization with no digest in the catalog is refused with [ErrUnpinned] rather than
// fetched on trust. A file already on disk is hashed and kept only if it matches; one that
// does not is replaced.
func (h *Host) EnsureWeights(ctx context.Context, id, quant string) (Weights, error) {
	m, err := h.Model(id)
	if err != nil {
		return Weights{}, h.fail(err)
	}
	q, err := pickQuant(m, quant)
	if err != nil {
		return Weights{}, err
	}
	w, err := h.weights(ctx, m, q)
	return w, h.fail(err)
}

func pickQuant(m Model, name string) (catalog.Quant, error) {
	if name == "" {
		return smallestServable(m)
	}
	for _, q := range m.Quants {
		if strings.EqualFold(q.Name, name) {
			return q, nil
		}
	}
	return catalog.Quant{}, fmt.Errorf("localmodel: %q has no quantization %q", m.ID, name)
}

// smallestServable returns the smallest quantization this package can serve: a single
// GGUF file. A model whose only quantizations are another format fails with the reason its
// smallest one cannot be served, instead of silently choosing nothing.
func smallestServable(m Model) (catalog.Quant, error) {
	var best catalog.Quant
	found := false
	for _, q := range m.Quants {
		if q.Format != catalog.FormatGGUF || q.MultiFile() {
			continue
		}
		if !found || q.SizeBytes < best.SizeBytes {
			best, found = q, true
		}
	}
	if found {
		return best, nil
	}
	q, ok := m.SmallestQuant()
	if !ok {
		return catalog.Quant{}, fmt.Errorf("localmodel: %q has no quantizations", m.ID)
	}
	_, err := servable(m, q)
	if err == nil {
		err = fmt.Errorf("%w: %q has no single-file GGUF quantization", ErrNotServable, m.ID)
	}
	return catalog.Quant{}, err
}

// servable checks that a quantization is one this package can fetch and serve, and
// returns its digest in bare hex.
func servable(m Model, q catalog.Quant) (string, error) {
	switch {
	case q.Format == catalog.FormatPickle:
		return "", fmt.Errorf("%w: %q quant %q uses a code-executing weight format", ErrNotServable, m.ID, q.Name)
	case q.MultiFile() || q.Format != catalog.FormatGGUF:
		return "", fmt.Errorf("%w: %q quant %q is not a single GGUF file", ErrNotServable, m.ID, q.Name)
	case q.URL == "":
		return "", fmt.Errorf("%w: %q quant %q has no direct download", ErrNotServable, m.ID, q.Name)
	}
	d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(q.Digest), "sha256:"))
	if d == "" {
		return "", fmt.Errorf("%w: %q quant %q", ErrUnpinned, m.ID, q.Name)
	}
	return d, nil
}

func (h *Host) weights(ctx context.Context, m Model, q catalog.Quant) (Weights, error) {
	digest, err := servable(m, q)
	if err != nil {
		return Weights{}, err
	}
	dest := h.dataDirPath("models", weightsFileName(m.ID, q))
	if _, err := os.Stat(dest); err == nil {
		got, hashErr := hashFile(dest)
		if hashErr == nil && got == digest {
			return Weights{Path: dest, Quant: q.Name, SHA256: digest}, nil
		}
		// A file that no longer matches is replaced, never served. It may be read-only
		// from an earlier launch, which a removal on some platforms refuses.
		_ = os.Chmod(dest, 0o600)
		if err := os.Remove(dest); err != nil {
			return Weights{}, fmt.Errorf("localmodel: remove stale weights: %w", err)
		}
	}
	h.say("fetching %s (%s, %d MiB)", m.ID, q.Name, q.SizeBytes>>20)
	res, err := h.dl.Fetch(ctx, fetch.Request{URL: q.URL, Dest: dest, ExpectSHA256: digest, MaxBytes: sizeCeiling(q.SizeBytes)})
	if err != nil {
		return Weights{}, fmt.Errorf("localmodel: fetch weights: %w", err)
	}
	return Weights{Path: res.Path, Quant: q.Name, SHA256: digest}, nil
}

// verifyWeights is the check [Host.Serve] makes immediately before launch, so a file
// swapped between provisioning and serving is caught at the last moment it can be.
func verifyWeights(w Weights) error {
	got, err := hashFile(w.Path)
	if err != nil {
		return fmt.Errorf("localmodel: hash weights: %w", err)
	}
	if got != w.SHA256 {
		return fmt.Errorf("%w: the weights changed after they were verified", ErrDigestMismatch)
	}
	return nil
}

// verifiedTemplate hashes the weights through one open file, refuses them unless the digest
// matches, and only then rewinds that same file and reads its header. Nothing is parsed from
// a path, so a file swapped for another after the hash is never what the parser sees.
func verifiedTemplate(w Weights, trusted string) (gguf.ChatTemplateDecision, error) {
	f, err := os.Open(w.Path) //nolint:gosec // a path under the data directory
	if err != nil {
		return gguf.ChatTemplateDecision{}, fmt.Errorf("localmodel: open weights: %w", err)
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return gguf.ChatTemplateDecision{}, fmt.Errorf("localmodel: hash weights: %w", err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != w.SHA256 {
		return gguf.ChatTemplateDecision{}, fmt.Errorf("%w: the weights changed after they were verified", ErrDigestMismatch)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return gguf.ChatTemplateDecision{}, fmt.Errorf("localmodel: rewind weights: %w", err)
	}
	return launch.InspectTemplateFrom(f, trusted)
}

// sizeCeiling caps a download at the declared size plus 5%, so a server cannot stream
// more than the model is supposed to be. Zero leaves the downloader's own ceiling.
func sizeCeiling(size int64) int64 {
	if size <= 0 {
		return 0
	}
	return size + size/20
}

// weightsFileName builds a filesystem-safe name from the model id and quantization, so
// two models never collide on disk.
func weightsFileName(id string, q catalog.Quant) string {
	safe := strings.NewReplacer("/", "_", ":", "_", "\\", "_", " ", "_").Replace(id)
	return safe + "-" + strings.NewReplacer("/", "_", " ", "_").Replace(q.Name) + ".gguf"
}

// keyFileName is the per-model token file name, in the same character set as weights.
func keyFileName(id string) string {
	return filepath.Base(strings.NewReplacer("/", "_", ":", "_", "\\", "_", " ", "_").Replace(id)) + ".key"
}
