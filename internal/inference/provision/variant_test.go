package provision

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/inference"
	"github.com/ionalpha/flynn/internal/inference/gpu"
)

// serveArchives serves each body at its path from one TLS server and returns the full URL
// of each path with a downloader that trusts the server.
func serveArchives(t *testing.T, bodies map[string][]byte) (*fetch.Downloader, map[string]string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	urls := make(map[string]string, len(bodies))
	for p := range bodies {
		urls[p] = srv.URL + p
	}
	return fetch.New(fetch.WithHTTPClient(srv.Client())), urls
}

func windowsReleases(t *testing.T) []Release {
	t.Helper()
	rels := ReleasesFor("llama.cpp", "windows", "amd64")
	if got := strings.Join(Variants(rels), ","); got != "cpu,cuda-13.3,cuda-12.4,vulkan" {
		t.Fatalf("windows/amd64 should pin cpu, two CUDA builds and vulkan, got %s", got)
	}
	return rels
}

func TestChoosePicksTheBuildTheHardwareCanRun(t *testing.T) {
	nv := func(cuda inference.Version, cc float64) gpu.Info {
		return gpu.Info{Name: "NVIDIA GeForce RTX", Driver: "1", CUDA: cuda, ComputeCap: cc}
	}
	tests := []struct {
		name string
		hw   gpu.Info
		want string
	}{
		{"no gpu", gpu.Info{}, "cpu"},
		{"newest driver gets the newest build", nv(inference.Version{13, 3}, 8.9), "cuda-13.3"},
		{"a newer driver still gets the newest pinned build", nv(inference.Version{14, 0}, 8.9), "cuda-13.3"},
		{"a driver older than 13.3 falls back to 12.4", nv(inference.Version{13, 0}, 8.9), "cuda-12.4"},
		{"a card CUDA 13 dropped gets 12.4", nv(inference.Version{13, 3}, 6.1), "cuda-12.4"},
		{"unreported compute capability is not risked on CUDA 13", nv(inference.Version{13, 3}, 0), "cuda-12.4"},
		{"a driver older than every build gets the cpu build", nv(inference.Version{11, 8}, 8.6), "cpu"},
		{"unknown driver CUDA gets the cpu build", nv(nil, 8.9), "cpu"},
		{"radeon gets vulkan", gpu.Info{Vulkan: "AMD Radeon RX 7800 XT"}, "vulkan"},
	}
	rels := windowsReleases(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Choose(rels, tc.hw)
			if !ok || got.Name() != tc.want {
				t.Fatalf("Choose = %q (ok=%v), want %q", got.Name(), ok, tc.want)
			}
		})
	}
}

func TestChooseNeverAutoSelectsVulkanOnLinux(t *testing.T) {
	rels := ReleasesFor("llama.cpp", "linux", "amd64")
	got, ok := Choose(rels, gpu.Info{Vulkan: "AMD Radeon RX 6700 XT"})
	if !ok || got.Name() != "cpu" {
		t.Fatalf("Choose = %q, want cpu: the Vulkan loader may be absent", got.Name())
	}
	if _, ok := ByVariant(rels, "vulkan"); !ok {
		t.Fatal("vulkan must stay selectable by name on linux")
	}
}

func TestChooseWithoutABaselineReportsNone(t *testing.T) {
	if _, ok := Choose(nil, gpu.Info{}); ok {
		t.Fatal("Choose invented a build from an empty set")
	}
}

func TestByVariantAndReleaseFor(t *testing.T) {
	rels := windowsReleases(t)
	base, ok := ReleaseFor("llama.cpp", "windows", "amd64")
	if !ok || base.Variant != "" || rels[0].Variant != "" {
		t.Fatalf("the baseline must lead and be what ReleaseFor returns: %+v", base)
	}
	if _, ok := ByVariant(rels, "cuda-9.9"); ok {
		t.Fatal("an unknown variant name matched")
	}
	if _, ok := ReleaseFor("llama.cpp", "plan9", "amd64"); ok {
		t.Fatal("a platform with no pin returned a build")
	}
}

func TestVariantsInstallToDistinctDirectories(t *testing.T) {
	dest := t.TempDir()
	seen := map[string]string{}
	for _, r := range Releases() {
		key := r.GOOS + "/" + r.GOARCH + " " + r.Dir(dest)
		if prev, dup := seen[key]; dup {
			t.Errorf("%s and %s share %s", prev, r.Name(), r.Dir(dest))
		}
		seen[key] = r.Name()
	}
	cpu := Release{Runtime: "llama.cpp", Version: inference.Version{9813}}
	if got, want := cpu.Dir(dest), filepath.Join(dest, "llama.cpp", "9813"); got != want {
		t.Errorf("the baseline layout moved: %s, want %s", got, want)
	}
}

func TestEveryPinnedVariantIsGatedAndDigested(t *testing.T) {
	for _, r := range Releases() {
		label := r.GOOS + "/" + r.GOARCH + " " + r.Name()
		if err := r.Gate(); err != nil {
			t.Errorf("%s fails the version gate: %v", label, err)
		}
		pins := []string{r.SHA256}
		for _, c := range r.Companions {
			pins = append(pins, c.SHA256)
			if !strings.HasPrefix(c.URL, "https://") {
				t.Errorf("%s: companion is not fetched over https", label)
			}
		}
		for _, p := range pins {
			if len(p) != 64 || strings.Trim(p, "0123456789abcdef") != "" {
				t.Errorf("%s has a malformed digest %q", label, p)
			}
		}
		if !strings.HasPrefix(r.URL, "https://") {
			t.Errorf("%s is not fetched over https", label)
		}
		if (r.Accel == AccelCUDA) != (len(r.CUDA) > 0) {
			t.Errorf("%s: CUDA version and accelerator disagree", label)
		}
		if r.Accel == AccelCUDA && len(r.Companions) == 0 {
			t.Errorf("%s: a CUDA build without its runtime libraries cannot start", label)
		}
		if r.Accel != AccelNone && r.Variant == "" {
			t.Errorf("%s: a GPU build needs a variant name", label)
		}
	}
}

func TestDownloadBytesCountsCompanions(t *testing.T) {
	r := Release{SizeBytes: 10, Companions: []Companion{{SizeBytes: 5}, {SizeBytes: 7}}}
	if got := r.DownloadBytes(); got != 22 {
		t.Fatalf("DownloadBytes = %d, want 22", got)
	}
}

func cudaFixture(t *testing.T, serverBytes, runtimeBytes, servedRuntime []byte) (Release, *fetch.Downloader) {
	t.Helper()
	dl, urls := serveArchives(t, map[string][]byte{"/main.zip": serverBytes, "/rt.zip": servedRuntime})
	return Release{
		Runtime: "llama.cpp", Version: llamaCppVersion, GOOS: "windows", GOARCH: "amd64",
		Variant: "cuda-13.3", Accel: AccelCUDA, CUDA: inference.Version{13, 3},
		URL: urls["/main.zip"], SHA256: sha256Hex(serverBytes), SizeBytes: int64(len(serverBytes)),
		Archive: ArchiveZip, BinName: "llama-server.exe",
		Companions: []Companion{{
			URL: urls["/rt.zip"], SHA256: sha256Hex(runtimeBytes), SizeBytes: int64(len(servedRuntime)), Archive: ArchiveZip,
		}},
	}, dl
}

func TestInstallPlacesACompanionBesideTheBinaryAndLookupFindsIt(t *testing.T) {
	dest := t.TempDir()
	server := buildZip(t, map[string]string{"llama-server.exe": "server"})
	rt := buildZip(t, map[string]string{"cudart64_13.dll": "runtime"})
	rel, dl := cudaFixture(t, server, rt, rt)

	if _, ok := Lookup(rel, dest); ok {
		t.Fatal("found a build before anything was installed")
	}
	inst, err := Install(context.Background(), dl, rel, dest)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Variant != "cuda-13.3" || inst.FromCache {
		t.Fatalf("install = %+v", inst)
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(inst.BinPath), "cudart64_13.dll")); err != nil || string(b) != "runtime" {
		t.Fatalf("the runtime library is not beside the server: %q %v", b, err)
	}
	found, ok := Lookup(rel, dest)
	if !ok || found.BinPath != inst.BinPath || found.Variant != "cuda-13.3" || !found.FromCache {
		t.Fatalf("Lookup = %+v ok=%v, want the path Install returned (%s)", found, ok, inst.BinPath)
	}
	cpu := rel
	cpu.Variant = ""
	if _, ok := Lookup(cpu, dest); ok {
		t.Fatal("a CUDA install was reported as the CPU build")
	}
}

func TestInstallRefusesATamperedCompanion(t *testing.T) {
	dest := t.TempDir()
	server := buildZip(t, map[string]string{"llama-server.exe": "server"})
	rt := buildZip(t, map[string]string{"cudart64_13.dll": "runtime"})
	evil := buildZip(t, map[string]string{"cudart64_13.dll": "tampered"})
	rel, dl := cudaFixture(t, server, rt, evil)

	if _, err := Install(context.Background(), dl, rel, dest); err == nil {
		t.Fatal("an archive that does not match its pinned digest was installed")
	}
	if _, ok := Lookup(rel, dest); ok {
		t.Fatal("a refused install left a runnable build behind")
	}
}

func TestInstallRefusesATamperedMainArchive(t *testing.T) {
	dest := t.TempDir()
	server := buildZip(t, map[string]string{"llama-server.exe": "server"})
	evil := buildZip(t, map[string]string{"llama-server.exe": "backdoor"})
	rt := buildZip(t, map[string]string{"cudart64_13.dll": "runtime"})
	rel, dl := cudaFixture(t, evil, rt, rt)
	rel.SHA256 = sha256Hex(server)

	if _, err := Install(context.Background(), dl, rel, dest); err == nil {
		t.Fatal("a main archive that does not match its pinned digest was installed")
	}
	if _, ok := Lookup(rel, dest); ok {
		t.Fatal("a refused install left a runnable build behind")
	}
}
