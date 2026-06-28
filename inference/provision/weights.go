package provision

import (
	"context"
	"fmt"
	"os"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/fetch"
)

// This file acquires a multi-file model: a directory of weight shards plus the tokenizer and
// config files a GPU runtime (vLLM) serves a model from. The single-file path (Install, for a
// GGUF runtime build, and the catalog's single-weights fetch) downloads one verified file; a
// safetensors model is instead a set of files served from a directory, so it needs the same
// verified-fetch discipline applied per file into one directory.
//
// Every file is fetched through the same hardened downloader (https-pinned, host-allowlisted,
// size-capped, digest-verified) and written under a single model directory with the same
// traversal guard the archive extractor uses, so a hostile manifest cannot place a file
// outside the model directory. A file already present is reused, so an interrupted fetch
// resumes by downloading only what is missing.

// ModelFile is one file of a multi-file model: its name within the model directory and where
// to fetch it from, verified against a pinned digest. It is the per-file unit a safetensors
// model's manifest is made of.
type ModelFile struct {
	// Name is the file's path within the model directory, for example "model.safetensors" or
	// "tokenizer.json". It is relative and must not escape the directory.
	Name string
	// URL is the https source of the file.
	URL string
	// SHA256 is the pinned digest the file is verified against. Empty pins on fetch (the
	// computed digest is recorded but nothing pre-pinned was checked), the lower-trust path
	// the single-file fetch also allows.
	SHA256 string
	// SizeBytes is the file's known size, used as the per-file download cap. 0 uses the
	// downloader's default ceiling.
	SizeBytes int64
}

// FetchModelDir downloads every file of a multi-file model into destDir, each verified and
// traversal-guarded, reusing any file already present. It returns destDir on success. It
// refuses an empty manifest (a model with no files is not a model) and an entry whose name
// escapes the directory, so the directory it returns holds exactly the manifest's files and
// nothing outside it. The download cap for a file is its known size plus a small margin, so a
// server that returns more than the manifest claims is refused rather than written.
func FetchModelDir(ctx context.Context, dl *fetch.Downloader, files []ModelFile, destDir string) (string, error) {
	if len(files) == 0 {
		return "", fault.New(fault.Terminal, "provision_no_files", "provision: a multi-file model has no files to fetch")
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return "", fault.Wrap(fault.Terminal, "provision_model_dir", err)
	}
	for _, f := range files {
		dst, ok := safeJoin(destDir, f.Name)
		if !ok {
			return "", traversalError(f.Name)
		}
		if _, err := os.Stat(dst); err == nil {
			continue // already fetched and verified on a previous run
		}
		maxBytes := int64(0)
		if f.SizeBytes > 0 {
			maxBytes = f.SizeBytes + (1 << 20) // the known size plus a small margin
		}
		if _, err := dl.Fetch(ctx, fetch.Request{
			URL:          f.URL,
			Dest:         dst,
			ExpectSHA256: f.SHA256,
			MaxBytes:     maxBytes,
		}); err != nil {
			return "", fault.Wrap(fault.Terminal, "provision_model_fetch",
				fmt.Errorf("provision: fetch %s: %w", f.Name, err))
		}
	}
	return destDir, nil
}

// ModelDirPresent reports whether every file of a manifest is already present under destDir,
// so a caller can skip the fetch entirely (and the network) when the model is fully on disk.
func ModelDirPresent(files []ModelFile, destDir string) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		dst, ok := safeJoin(destDir, f.Name)
		if !ok {
			return false
		}
		if _, err := os.Stat(dst); err != nil {
			return false
		}
	}
	return true
}
