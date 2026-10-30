package provision

import (
	"github.com/ionalpha/flynn/internal/inference"
	"github.com/ionalpha/flynn/internal/inference/gpu"
)

// llamaCppVersion is the pinned llama.cpp build Flynn installs. It is above the
// runtime's minimum-supported floor, so every entry below passes the version gate. The
// digests are the published sha256 of each release archive; the download is refused
// unless the bytes match.
var llamaCppVersion = inference.Version{9813}

// llamaCppAsset is where the pinned build's release assets are published.
const llamaCppAsset = "https://github.com/ggml-org/llama.cpp/releases/download/b9813/" //nolint:gosec // a public release URL prefix, not a credential

// releases is the fixed set of runtime builds Flynn will fetch and install, one per
// supported platform. It covers llama.cpp, the runtime Flynn provisions itself when no
// runtime is present: its release archives are small, ship for every common platform,
// and use only zip and gzip, so installing one adds no decompression dependency and no
// large download. A present system runtime is preferred over fetching one; this set is
// the fallback that makes a clean machine able to run a model with no manual setup.
//
// Each entry pins the archive URL, its size, and its sha256. Changing the pinned build
// means updating these values together, so the set of bytes Flynn will execute is
// always explicit and reviewable.
var releases = []Release{
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "windows", GOARCH: "amd64",
		URL:       "https://github.com/ggml-org/llama.cpp/releases/download/b9813/llama-b9813-bin-win-cpu-x64.zip",
		SHA256:    "8751c5bb36923a705327902c1c9822ab41103394aad5084fc05fdbaac7bee611",
		SizeBytes: 17383414, Archive: ArchiveZip, BinName: "llama-server.exe",
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "windows", GOARCH: "arm64",
		URL:       "https://github.com/ggml-org/llama.cpp/releases/download/b9813/llama-b9813-bin-win-cpu-arm64.zip",
		SHA256:    "6e43cfee3d4841330f54158abbb37e6a1935993e2e3e8c84b10f75828f070bb6",
		SizeBytes: 11291711, Archive: ArchiveZip, BinName: "llama-server.exe",
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "linux", GOARCH: "amd64",
		URL:       "https://github.com/ggml-org/llama.cpp/releases/download/b9813/llama-b9813-bin-ubuntu-x64.tar.gz",
		SHA256:    "1a96ca8bc662e1059c5eeb2a239aa1df8589bb4d0f43f431f359cac6138dda84",
		SizeBytes: 15743920, Archive: ArchiveTarGz, BinName: "llama-server",
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "linux", GOARCH: "arm64",
		URL:       "https://github.com/ggml-org/llama.cpp/releases/download/b9813/llama-b9813-bin-ubuntu-arm64.tar.gz",
		SHA256:    "1b728989db84f823f9c2b04d9eb738a51274f18d861e10384696d0ea444d779b",
		SizeBytes: 12748547, Archive: ArchiveTarGz, BinName: "llama-server",
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "darwin", GOARCH: "arm64",
		URL:       "https://github.com/ggml-org/llama.cpp/releases/download/b9813/llama-b9813-bin-macos-arm64.tar.gz",
		SHA256:    "5074a1de3985cd31b86e6198888761b56f86a7ee00c368df191d29ea84b74138",
		SizeBytes: 11047394, Archive: ArchiveTarGz, BinName: "llama-server",
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "darwin", GOARCH: "amd64",
		URL:       "https://github.com/ggml-org/llama.cpp/releases/download/b9813/llama-b9813-bin-macos-x64.tar.gz",
		SHA256:    "39d4f49f6d93a8df3fda6cc28ea104eeb0450e87cd5bd63966669794bb578c6d",
		SizeBytes: 11349732, Archive: ArchiveTarGz, BinName: "llama-server",
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "windows", GOARCH: "amd64",
		Variant: "cuda-13.3", Accel: AccelCUDA, CUDA: inference.Version{13, 3}, MinComputeCap: 7.5,
		URL:       llamaCppAsset + "llama-b9813-bin-win-cuda-13.3-x64.zip",
		SHA256:    "5525af7d44c5871632f60bf98e6c86e6ba28776e4a2ae0b2ea032017da2b6fd0",
		SizeBytes: 159670269, Archive: ArchiveZip, BinName: "llama-server.exe",
		Companions: []Companion{{
			URL:       llamaCppAsset + "cudart-llama-bin-win-cuda-13.3-x64.zip",
			SHA256:    "1462a050eb4c684921ba51dcc4cc488a036674c3e73e9945ee705b854808d03e",
			SizeBytes: 390970417, Archive: ArchiveZip,
		}},
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "windows", GOARCH: "amd64",
		Variant: "cuda-12.4", Accel: AccelCUDA, CUDA: inference.Version{12, 4},
		URL:       llamaCppAsset + "llama-b9813-bin-win-cuda-12.4-x64.zip",
		SHA256:    "147f5dd6098fafb802d2f3da67df380462a5f1b461113cfad71f84df9c151e61",
		SizeBytes: 261777334, Archive: ArchiveZip, BinName: "llama-server.exe",
		Companions: []Companion{{
			URL:       llamaCppAsset + "cudart-llama-bin-win-cuda-12.4-x64.zip",
			SHA256:    "8c79a9b226de4b3cacfd1f83d24f962d0773be79f1e7b75c6af4ded7e32ae1d6",
			SizeBytes: 391443627, Archive: ArchiveZip,
		}},
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "windows", GOARCH: "amd64",
		Variant: "vulkan", Accel: AccelVulkan,
		URL:       llamaCppAsset + "llama-b9813-bin-win-vulkan-x64.zip",
		SHA256:    "9435916bbec47274a7dff5731fad8f93d8495fb35d33a37e3505957d27b2ca39",
		SizeBytes: 31961098, Archive: ArchiveZip, BinName: "llama-server.exe",
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "linux", GOARCH: "amd64",
		Variant: "vulkan", Accel: AccelVulkan,
		URL:       llamaCppAsset + "llama-b9813-bin-ubuntu-vulkan-x64.tar.gz",
		SHA256:    "e2f15f6a8a50bc2588a8f88878df1f6cb0ebd1a23b5ec38dcf3bf8eae618e138",
		SizeBytes: 30996251, Archive: ArchiveTarGz, BinName: "llama-server",
	},
	{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "linux", GOARCH: "arm64",
		Variant: "vulkan", Accel: AccelVulkan,
		URL:       llamaCppAsset + "llama-b9813-bin-ubuntu-vulkan-arm64.tar.gz",
		SHA256:    "aafe4c03cb213e7bcf67cc3880406193a8f77583a6a43dac30ac48ea74ba945a",
		SizeBytes: 25396897, Archive: ArchiveTarGz, BinName: "llama-server",
	},
}

// ReleaseFor returns the pinned CPU baseline Flynn would install for a platform, and
// whether one exists. The runtime is matched by name (matching inference.Runtime.Name),
// the platform by Go's runtime.GOOS and runtime.GOARCH. The baseline runs on every
// machine of the platform, which is why callers that have not looked at the hardware use
// it; [Choose] and [ReleasesFor] are for those that have.
func ReleaseFor(runtime, goos, goarch string) (Release, bool) {
	for _, r := range ReleasesFor(runtime, goos, goarch) {
		if r.Variant == "" {
			return r, true
		}
	}
	return Release{}, false
}

// ReleasesFor returns every pinned build of a runtime for a platform, the CPU baseline
// first, then the GPU variants in the order they are pinned.
func ReleasesFor(runtime, goos, goarch string) []Release {
	var out []Release
	for _, r := range releases {
		if r.Runtime == runtime && r.GOOS == goos && r.GOARCH == goarch {
			if r.Variant == "" {
				out = append([]Release{r}, out...)
			} else {
				out = append(out, r)
			}
		}
	}
	return out
}

// Releases returns a copy of the full pinned release set, for reporting which builds
// Flynn can install.
func Releases() []Release { return append([]Release(nil), releases...) }

// Variants are the names a user can pass to select a build of a platform: "cpu" for the
// baseline, then each GPU variant.
func Variants(rels []Release) []string {
	names := make([]string, 0, len(rels))
	for _, r := range rels {
		names = append(names, r.Name())
	}
	return names
}

// ByVariant finds the build named name ("cpu" selects the baseline) among rels.
func ByVariant(rels []Release, name string) (Release, bool) {
	for _, r := range rels {
		if r.Name() == name {
			return r, true
		}
	}
	return Release{}, false
}

// Choose picks the build of rels (one runtime, one platform) that makes best use of the
// hardware in hw, falling back to the CPU baseline.
//
// A CUDA build is offered only to an NVIDIA GPU whose driver reports a CUDA version at least
// the build's, because a build made with a newer toolkit than the driver supports does not
// load, and the failure arrives at launch rather than at install. The newest such build
// wins. A build that dropped older architectures (MinComputeCap) is withheld from a card it
// no longer targets, and from a card whose compute capability the driver did not report.
//
// Vulkan is chosen on Windows when an AMD or Intel Arc adapter was found and no CUDA build
// fits. It is not chosen on Linux, where the Vulkan loader is a separate package a machine
// may lack and the build would then fail to start; a Linux user asks for it by name.
func Choose(rels []Release, hw gpu.Info) (Release, bool) {
	var best *Release
	for i := range rels {
		r := &rels[i]
		if r.Accel != AccelCUDA || !cudaFits(*r, hw) {
			continue
		}
		if best == nil || best.CUDA.Less(r.CUDA) {
			best = r
		}
	}
	if best != nil {
		return *best, true
	}
	if hw.Vulkan != "" {
		for _, r := range rels {
			if r.Accel == AccelVulkan && r.GOOS == "windows" {
				return r, true
			}
		}
	}
	return ByVariant(rels, "cpu")
}

// cudaFits reports whether the CUDA build r can load on the NVIDIA GPU hw describes.
func cudaFits(r Release, hw gpu.Info) bool {
	if !hw.NVIDIA() || len(hw.CUDA) == 0 || hw.CUDA.Less(r.CUDA) {
		return false
	}
	return r.MinComputeCap == 0 || hw.ComputeCap >= r.MinComputeCap
}
