package provision

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/inference"
)

// TestProvisionersNameTheirRuntime checks each strategy reports the runtime it provisions,
// which is what routes an acquisition to the right launch plan.
func TestProvisionersNameTheirRuntime(t *testing.T) {
	archive := ArchiveProvisioner{Release: Release{Runtime: "llama.cpp"}}
	if archive.Runtime() != "llama.cpp" {
		t.Errorf("archive runtime = %q, want llama.cpp", archive.Runtime())
	}
	detect := DetectProvisioner{RuntimeName: "llama.cpp"}
	if detect.Runtime() != "llama.cpp" {
		t.Errorf("detect runtime = %q, want llama.cpp", detect.Runtime())
	}
	container := ContainerProvisioner{RuntimeName: "vllm"}
	if container.Runtime() != "vllm" {
		t.Errorf("container runtime = %q, want vllm", container.Runtime())
	}
}

// TestArchiveProvisionerReportsAFailedInstall checks an archive that cannot be installed is
// an error rather than an Acquired naming a binary that is not there.
func TestArchiveProvisionerReportsAFailedInstall(t *testing.T) {
	p := ArchiveProvisioner{
		Release: Release{
			Runtime: "llama.cpp",
			Version: inference.Version{9, 9, 9},
			URL:     "https://127.0.0.1:1/nothing.tar.gz",
			SHA256:  strings.Repeat("a", 64),
			Archive: ArchiveTarGz,
			BinName: "llama-server",
		},
		DestDir:    t.TempDir(),
		Downloader: fetch.New(),
	}
	if _, err := p.Acquire(context.Background()); err == nil {
		t.Fatal("an archive that cannot be downloaded must fail the acquisition")
	}
}

// TestVLLMImageIsPinnedAndGated checks the blessed vLLM image is a digest-pinned reference
// whose version passes the advisory floor, and that it builds a provisioner carrying that
// exact pin: the digest is the whole trust anchor for the container stack.
func TestVLLMImageIsPinnedAndGated(t *testing.T) {
	img := VLLMImage()
	if img.Ref == "" {
		t.Error("the blessed image has no reference")
	}
	if !strings.HasPrefix(img.Digest, "sha256:") || len(img.Digest) != len("sha256:")+64 {
		t.Errorf("digest = %q, want a sha256 content digest", img.Digest)
	}
	if err := img.Gate(); err != nil {
		t.Errorf("the blessed image fails the advisory floor: %v", err)
	}

	var pulled string
	p := img.Provisioner(func(_ context.Context, ref, digest string) error {
		pulled = ref + "@" + digest
		return nil
	})
	if p.Runtime() != "vllm" || p.Ref != img.Ref || p.Digest != img.Digest || p.Version.String() != img.Version.String() {
		t.Fatalf("provisioner = %+v, want the blessed image's pin", p)
	}

	got, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.Image.Digest != img.Digest {
		t.Errorf("acquired digest = %q, want the pinned one", got.Image.Digest)
	}
	if pulled != img.Ref+"@"+img.Digest {
		t.Errorf("pulled %q, want the image pulled by digest", pulled)
	}

	// A nil puller drives an image the engine already has, so the acquisition still reports
	// the pin without pulling anything.
	cached, err := img.Provisioner(nil).Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire with no puller: %v", err)
	}
	if cached.Image.Digest != img.Digest {
		t.Errorf("acquired digest = %q, want the pinned one", cached.Image.Digest)
	}
}

// TestReleaseForReportsAnUnservedPlatform checks the pinned-build lookup says a platform has
// no build rather than handing back a zero Release that would be installed as if it were one.
func TestReleaseForReportsAnUnservedPlatform(t *testing.T) {
	if _, ok := ReleaseFor("llama.cpp", "plan9", "riscv64"); ok {
		t.Error("a platform with no pinned build must report none")
	}
	if _, ok := ReleaseFor("no-such-runtime", runtime.GOOS, runtime.GOARCH); ok {
		t.Error("a runtime with no pinned build must report none")
	}
	// The set is not empty, so the refusals above are the lookup working rather than a
	// lookup that never finds anything.
	if len(Releases()) == 0 {
		t.Fatal("no pinned releases at all")
	}
}

// TestFetchModelDirReportsAnUnusableDestination checks a model directory that cannot be
// created is an error before anything is downloaded.
func TestFetchModelDirReportsAnUnusableDestination(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	files := []ModelFile{{Name: "a.safetensors", URL: "https://127.0.0.1:1/a", SHA256: strings.Repeat("a", 64)}}

	if _, err := FetchModelDir(context.Background(), fetch.New(), files, filepath.Join(file, "sub")); err == nil {
		t.Fatal("a destination that cannot be made must fail before any download")
	}
}

// TestModelDirPresentIsFalseForAnUnusableManifest checks the on-disk check refuses to report
// a model as present when the manifest is empty or names a path that escapes the directory:
// either would skip the fetch of a model that is not really there.
func TestModelDirPresentIsFalseForAnUnusableManifest(t *testing.T) {
	dir := t.TempDir()
	if ModelDirPresent(nil, dir) {
		t.Error("an empty manifest must never read as present")
	}
	if ModelDirPresent([]ModelFile{{Name: "../escape.bin"}}, dir) {
		t.Error("a manifest naming a path outside the directory must never read as present")
	}
}
