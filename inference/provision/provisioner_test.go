package provision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/inference"
)

// pinnedTestDigest is a well-formed sha256 image digest for building container specs.
const pinnedTestDigest = "sha256:" +
	"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func TestArchiveProvisionerAcquiresBinary(t *testing.T) {
	archive := buildZip(t, map[string]string{"llama-server.exe": "#!binary", "ggml.dll": "lib"})
	url, dl := serveArchive(t, archive)
	p := ArchiveProvisioner{
		Release: Release{
			Runtime: "llama.cpp", Version: inference.Version{9813}, GOOS: "windows", GOARCH: "amd64",
			URL: url, SHA256: sha256Hex(archive), SizeBytes: int64(len(archive)),
			Archive: ArchiveZip, BinName: "llama-server.exe",
		},
		DestDir:    t.TempDir(),
		Downloader: dl,
	}
	if p.Runtime() != "llama.cpp" {
		t.Fatalf("runtime name %q", p.Runtime())
	}
	got, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.IsContainer() || got.Binary == "" || got.FromCache {
		t.Fatalf("archive acquire should yield a fresh binary, got %+v", got)
	}
	if !strings.HasSuffix(got.Binary, "llama-server.exe") {
		t.Fatalf("binary path %q", got.Binary)
	}
}

func TestContainerProvisionerPullsAndPins(t *testing.T) {
	var pulledRef, pulledDigest string
	p := ContainerProvisioner{
		RuntimeName: "vllm", Version: inference.Version{0, 11, 1},
		Ref: "vllm/vllm-openai:v0.11.1", Digest: pinnedTestDigest,
		Pull: func(_ context.Context, ref, digest string) error {
			pulledRef, pulledDigest = ref, digest
			return nil
		},
	}
	got, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsContainer() || got.Binary != "" {
		t.Fatalf("container acquire should yield an image, got %+v", got)
	}
	if got.Image.Digest != pinnedTestDigest || got.FromCache {
		t.Fatalf("expected a freshly pulled pinned image, got %+v", got)
	}
	if pulledRef != p.Ref || pulledDigest != pinnedTestDigest {
		t.Fatalf("puller called with %q@%q", pulledRef, pulledDigest)
	}
}

func TestContainerProvisionerNilPullerIsCacheHit(t *testing.T) {
	// With no puller the strategy drives an image the engine already has: no pull, marked
	// from cache, but still gated and pinned.
	p := ContainerProvisioner{
		RuntimeName: "vllm", Version: inference.Version{0, 11, 1},
		Ref: "vllm/vllm-openai:v0.11.1", Digest: pinnedTestDigest,
	}
	got, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.FromCache || !got.IsContainer() {
		t.Fatalf("nil puller should be a container cache hit, got %+v", got)
	}
}

func TestContainerProvisionerRefusesVulnerableBeforePull(t *testing.T) {
	// A digest blessed as a sub-floor version must be refused before the image is pulled:
	// a puller that is reached is the bug.
	p := ContainerProvisioner{
		RuntimeName: "vllm", Version: inference.Version{0, 10, 0},
		Ref: "vllm/vllm-openai:v0.10.0", Digest: pinnedTestDigest,
		Pull: func(context.Context, string, string) error {
			t.Fatal("a sub-floor version must be refused without pulling")
			return nil
		},
	}
	if _, err := p.Acquire(context.Background()); err == nil {
		t.Fatal("expected a sub-floor container version to be refused")
	}
}

func TestContainerProvisionerRefusesUnpinned(t *testing.T) {
	for name, digest := range map[string]string{
		"empty":     "",
		"tag":       "latest",
		"short":     "sha256:abcd",
		"wrong alg": "md5:" + strings.Repeat("a", 64),
		"non-hex":   "sha256:" + strings.Repeat("z", 64),
	} {
		t.Run(name, func(t *testing.T) {
			p := ContainerProvisioner{
				RuntimeName: "vllm", Version: inference.Version{0, 11, 1},
				Ref: "vllm/vllm-openai:v0.11.1", Digest: digest,
				Pull: func(context.Context, string, string) error {
					t.Fatal("an unpinned image must be refused without pulling")
					return nil
				},
			}
			if _, err := p.Acquire(context.Background()); err == nil {
				t.Fatalf("expected unpinned digest %q to be refused", digest)
			}
		})
	}
}

func TestContainerProvisionerWrapsPullError(t *testing.T) {
	sentinel := errors.New("registry unreachable")
	p := ContainerProvisioner{
		RuntimeName: "vllm", Version: inference.Version{0, 11, 1},
		Ref: "vllm/vllm-openai:v0.11.1", Digest: pinnedTestDigest,
		Pull: func(context.Context, string, string) error { return sentinel },
	}
	if _, err := p.Acquire(context.Background()); err == nil {
		t.Fatal("a failed pull must surface an error")
	}
}

func TestDetectProvisionerAcquiresPresentBinary(t *testing.T) {
	p := DetectProvisioner{
		RuntimeName: "llama.cpp",
		Locate: func(context.Context) (string, string, bool) {
			return "/usr/bin/llama-server", "version: 9999 (abc123)", true
		},
	}
	got, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Binary != "/usr/bin/llama-server" || !got.FromCache {
		t.Fatalf("detect should report the present binary from cache, got %+v", got)
	}
	if got.Version.Less(inference.Version{9999}) || (inference.Version{9999}).Less(got.Version) {
		t.Fatalf("parsed version %v, want 9999", got.Version)
	}
}

func TestDetectProvisionerRefusesAbsent(t *testing.T) {
	p := DetectProvisioner{
		RuntimeName: "vllm",
		Locate:      func(context.Context) (string, string, bool) { return "", "", false },
	}
	if _, err := p.Acquire(context.Background()); err == nil {
		t.Fatal("an absent runtime must be refused")
	}
}

func TestDetectProvisionerGatesPresentVersion(t *testing.T) {
	// A present but sub-floor vLLM must still be refused: being installed is not being safe.
	p := DetectProvisioner{
		RuntimeName: "vllm",
		Locate:      func(context.Context) (string, string, bool) { return "/usr/bin/vllm", "vllm 0.9.0", true },
	}
	if _, err := p.Acquire(context.Background()); err == nil {
		t.Fatal("a present sub-floor version must be refused")
	}
}

func TestDetectProvisionerUnknownRuntime(t *testing.T) {
	p := DetectProvisioner{
		RuntimeName: "not-a-runtime",
		Locate:      func(context.Context) (string, string, bool) { return "/x", "1.0.0", true },
	}
	if _, err := p.Acquire(context.Background()); err == nil {
		t.Fatal("an unknown runtime must be refused")
	}
}

func TestDetectProvisionerNoLocator(t *testing.T) {
	p := DetectProvisioner{RuntimeName: "vllm"}
	if _, err := p.Acquire(context.Background()); err == nil {
		t.Fatal("a detect strategy with no locator must error")
	}
}

func TestPinnedDigest(t *testing.T) {
	cases := map[string]bool{
		pinnedTestDigest:                    true,
		"sha256:" + strings.Repeat("A", 64): true, // uppercase hex is well-formed
		"":                                  false,
		"latest":                            false,
		"sha256:abcd":                       false,
		"sha512:" + strings.Repeat("a", 64): false,
		"sha256:" + strings.Repeat("z", 64): false,
	}
	for d, want := range cases {
		if got := pinnedDigest(d); got != want {
			t.Fatalf("pinnedDigest(%q) = %v, want %v", d, got, want)
		}
	}
}
