package provision

import (
	"context"
	"fmt"
	"os"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/acquire"
	"github.com/ionalpha/flynn/internal/fetch"
)

// ModelFile is one file of a multi-file model such as a safetensors directory: its name
// in the model directory, its source, and its pinned digest.
type ModelFile struct {
	// Name is the file's path within the model directory, such as "tokenizer.json". It is
	// relative and must not escape the directory.
	Name string
	// URL is the https source of the file.
	URL string
	// SHA256 is the pinned digest. Empty pins on fetch: the computed digest is recorded
	// but nothing was checked, the lower-trust path the single-file fetch also allows.
	SHA256 string
	// SizeBytes is the file's known size, used as the download cap. 0 uses the
	// downloader's default ceiling.
	SizeBytes int64
}

// FetchModelDir downloads every file of a multi-file model into destDir through the
// hardened downloader, reusing files already present so an interrupted fetch resumes.
// It refuses an empty manifest and any name that escapes the directory, so destDir
// holds exactly the manifest's files. The cap for a file is its known size plus 1 MiB,
// so a server that sends more than the manifest claims is refused.
func FetchModelDir(ctx context.Context, dl *fetch.Downloader, files []ModelFile, destDir string) (string, error) {
	if len(files) == 0 {
		return "", fault.New(fault.Terminal, "provision_no_files", "provision: a multi-file model has no files to fetch")
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return "", fault.Wrap(fault.Terminal, "provision_model_dir", err)
	}
	for _, f := range files {
		dst, ok := acquire.SafeJoin(destDir, f.Name)
		if !ok {
			return "", acquire.TraversalError(f.Name)
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

// ModelDirPresent reports whether every file of a manifest is already under destDir, so
// a caller can skip the fetch and the network.
func ModelDirPresent(files []ModelFile, destDir string) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		dst, ok := acquire.SafeJoin(destDir, f.Name)
		if !ok {
			return false
		}
		if _, err := os.Stat(dst); err != nil {
			return false
		}
	}
	return true
}
