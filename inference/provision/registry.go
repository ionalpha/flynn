package provision

import "github.com/ionalpha/flynn/inference"

// llamaCppVersion is the pinned llama.cpp build Flynn installs. It is above the
// runtime's minimum-supported floor, so every entry below passes the version gate. The
// digests are the published sha256 of each release archive; the download is refused
// unless the bytes match.
var llamaCppVersion = inference.Version{9813}

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
}

// ReleaseFor returns the pinned runtime build Flynn would install for a platform, and
// whether one exists. The runtime is matched by name (matching inference.Runtime.Name),
// the platform by Go's runtime.GOOS and runtime.GOARCH.
func ReleaseFor(runtime, goos, goarch string) (Release, bool) {
	for _, r := range releases {
		if r.Runtime == runtime && r.GOOS == goos && r.GOARCH == goarch {
			return r, true
		}
	}
	return Release{}, false
}

// Releases returns a copy of the full pinned release set, for reporting which builds
// Flynn can install.
func Releases() []Release { return append([]Release(nil), releases...) }
