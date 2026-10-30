package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/inference/provision"
	"github.com/ionalpha/flynn/localmodel"
	"github.com/ionalpha/flynn/sandbox"
)

func ownStatus(st localmodel.RuntimeStatus) ownRuntimeStatus {
	return func(context.Context) (localmodel.RuntimeStatus, error) { return st, nil }
}

// llamaLine returns the llama.cpp entry of a report with the indented lines that follow it.
func llamaLine(t *testing.T, report string) string {
	t.Helper()
	var block []string
	in := false
	for _, line := range strings.Split(report, "\n") {
		entry := strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ")
		switch {
		case entry && strings.HasPrefix(strings.TrimSpace(line), localmodel.Runtime+" "):
			in = true
		case entry:
			in = false
		}
		if in {
			block = append(block, line)
		}
	}
	if len(block) == 0 {
		t.Fatalf("no llama.cpp entry in:\n%s", report)
	}
	return strings.Join(block, "\n")
}

func TestReportRuntimesShowsTheInstalledVariantAndGPUUse(t *testing.T) {
	cuda := localmodel.RuntimeStatus{
		Installed: true, Version: "9813", Variant: "cuda-13.3", BinPath: "x",
		GPU: "NVIDIA GeForce RTX 4070 Ti", UsesGPU: true, Variants: []string{"cpu", "cuda-13.3"},
	}
	cpuOnGPU := cuda
	cpuOnGPU.Variant, cpuOnGPU.UsesGPU = "cpu", false
	noGPU := cpuOnGPU
	noGPU.GPU = ""
	corrupt := cuda
	corrupt.Problem = "the runtime binary changed after it was installed"

	tests := []struct {
		name string
		st   localmodel.RuntimeStatus
		want []string
	}{
		{"cuda build on an nvidia gpu", cuda, []string{"ok", "variant: cuda-13.3", "NVIDIA GeForce RTX 4070 Ti, used by this build"}},
		{"cpu build on a machine with a gpu", cpuOnGPU, []string{"variant: cpu", "not used by this build", "--variant"}},
		{"cpu build and no gpu", noGPU, []string{"variant: cpu", "none detected"}},
		{"changed binary", corrupt, []string{"CORRUPT", "changed after it was installed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sb := &scriptedSandbox{replies: map[string]sandbox.ExecResult{}}
			var out bytes.Buffer
			if err := reportRuntimesWith(t.Context(), sb, ownStatus(tc.st), &out); err != nil {
				t.Fatal(err)
			}
			block := llamaLine(t, out.String())
			if strings.Contains(block, "not installed") {
				t.Fatalf("a runtime flynn installed was reported missing:\n%s", out.String())
			}
			for _, w := range tc.want {
				if !strings.Contains(block, w) {
					t.Errorf("report lacks %q:\n%s", w, block)
				}
			}
		})
	}
}

// A PATH binary is not consulted for the runtime Flynn installed: the run path uses the
// installed one, so the report must describe that one.
func TestReportRuntimesPrefersTheInstalledBuildOverPATH(t *testing.T) {
	sb := &scriptedSandbox{replies: map[string]sandbox.ExecResult{
		"llama-server --version": {Output: "version: 3000 (abc1234)"},
	}}
	st := localmodel.RuntimeStatus{Installed: true, Version: "9813", Variant: "cpu"}
	var out bytes.Buffer
	if err := reportRuntimesWith(t.Context(), sb, ownStatus(st), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "VULNERABLE") {
		t.Fatalf("the old PATH binary was reported instead of the installed build:\n%s", out.String())
	}
}

func TestReportRuntimesNamesTheBuildAnInstallWouldFetch(t *testing.T) {
	sb := &scriptedSandbox{replies: map[string]sandbox.ExecResult{}}
	st := localmodel.RuntimeStatus{Version: "9813", Variant: "cuda-13.3"}
	var out bytes.Buffer
	if err := reportRuntimesWith(t.Context(), sb, ownStatus(st), &out); err != nil {
		t.Fatal(err)
	}
	block := llamaLine(t, out.String())
	if !strings.Contains(block, "not installed") || !strings.Contains(block, "cuda-13.3") {
		t.Fatalf("an absent runtime should say which build install would fetch:\n%s", block)
	}
}

func TestReportRuntimesFallsBackWhenTheStatusFails(t *testing.T) {
	sb := &scriptedSandbox{replies: map[string]sandbox.ExecResult{"llama-server --version": {Output: "version: 9999 (feedface)"}}}
	failing := func(context.Context) (localmodel.RuntimeStatus, error) {
		return localmodel.RuntimeStatus{}, errors.New("no pinned build")
	}
	var out bytes.Buffer
	if err := reportRuntimesWith(t.Context(), sb, failing, &out); err != nil {
		t.Fatal(err)
	}
	if block := llamaLine(t, out.String()); !strings.Contains(block, "ok") {
		t.Fatalf("a PATH binary must still be reported when the status cannot be read:\n%s", block)
	}
}

// tarGzOf packs one executable file into a gzip-compressed tar archive.
func tarGzOf(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installLikeTheCommand places a runtime build under dataDir the way `models install` does,
// from a local origin in place of the release server, at the directory the pinned build
// installs to. The binary is a stand-in; what the test is about is where check looks.
func installLikeTheCommand(t *testing.T, dataDir string) string {
	t.Helper()
	rel, ok := provision.ReleaseFor(localmodel.Runtime, runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("no pinned %s build for %s/%s", localmodel.Runtime, runtime.GOOS, runtime.GOARCH)
	}
	body := []byte("stand-in server binary")
	archive := tarGzOf(t, rel.BinName, body)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(srv.Close)

	sum := sha256.Sum256(archive)
	rel.URL, rel.SHA256, rel.SizeBytes, rel.Archive = srv.URL+"/runtime.tar.gz", hex.EncodeToString(sum[:]), int64(len(archive)), provision.ArchiveTarGz
	inst, err := provision.Install(t.Context(), fetch.New(fetch.WithHTTPClient(srv.Client())), rel, provision.RuntimesDir(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	binSum := sha256.Sum256(body)
	if err := os.WriteFile(inst.BinPath+".sha256", []byte(hex.EncodeToString(binSum[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return inst.BinPath
}

// Regression: `flynn models check` said llama.cpp was not installed right after
// `flynn models install llama.cpp` succeeded, because it looked for a binary on PATH and
// never in the data directory the install wrote to.
func TestCheckReportsTheRuntimeInstallPlacedInTheDataDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	installLikeTheCommand(t, dataDir)

	var out bytes.Buffer
	if err := runRuntimeCheckAt(dataDir, &out); err != nil {
		t.Fatal(err)
	}
	block := llamaLine(t, out.String())
	if strings.Contains(block, "not installed") {
		t.Fatalf("check does not see the runtime install placed:\n%s", out.String())
	}
	if !strings.Contains(block, "variant: cpu (installed by flynn)") {
		t.Errorf("check should name the installed variant:\n%s", block)
	}
}

func TestCheckInAnEmptyDataDirectoryStillSaysNotInstalled(t *testing.T) {
	var out bytes.Buffer
	if err := runRuntimeCheckAt(filepath.Join(t.TempDir(), "data"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "local inference runtimes:\n") {
		t.Fatalf("missing the report header:\n%s", out.String())
	}
}

func TestInstallRefusesAVariantItDoesNotPin(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"after the runtime", []string{"llama.cpp", "--variant", "cuda-9.9"}, "cuda-9.9"},
		{"alone", []string{"--variant=nonsense"}, "nonsense"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runRuntimeInstall(tc.args, filepath.Join(t.TempDir(), "data"), &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "cpu") {
				t.Fatalf("err = %v; want a refusal naming %q and the valid variants", err, tc.want)
			}
		})
	}
}

// An installed build below the advisory floor is reported as vulnerable, as a PATH one is.
func TestReportRuntimesFlagsAnInstalledBuildWithAKnownAdvisory(t *testing.T) {
	sb := &scriptedSandbox{replies: map[string]sandbox.ExecResult{}}
	st := localmodel.RuntimeStatus{Installed: true, Version: "3000", Variant: "cpu", BinPath: "x"}
	var out bytes.Buffer
	if err := reportRuntimesWith(t.Context(), sb, ownStatus(st), &out); err != nil {
		t.Fatal(err)
	}
	if block := llamaLine(t, out.String()); !strings.Contains(block, "VULNERABLE") {
		t.Fatalf("an installed build with a known advisory was not flagged:\n%s", block)
	}
}

// fakeInstaller scripts what a Host reports before and after an install.
type fakeInstaller struct {
	status    localmodel.RuntimeStatus
	statusErr error
	result    localmodel.RuntimeInstall
	installed error
	asked     string
}

func (f *fakeInstaller) RuntimeStatus(context.Context) (localmodel.RuntimeStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeInstaller) InstallRuntime(_ context.Context, variant string) (localmodel.RuntimeInstall, error) {
	f.asked = variant
	return f.result, f.installed
}

func TestInstallRuntimeTellsTheUserWhichBuildWasChosen(t *testing.T) {
	variants := []string{"cpu", "cuda-13.3"}
	tests := []struct {
		name   string
		f      fakeInstaller
		asked  string
		want   []string
		absent []string
	}{
		{
			"cuda build on an nvidia gpu",
			fakeInstaller{
				status: localmodel.RuntimeStatus{GPU: "NVIDIA GeForce RTX 4070 Ti", UsesGPU: true, Variant: "cuda-13.3", Variants: variants},
				result: localmodel.RuntimeInstall{BinPath: "/r/llama-server", Variant: "cuda-13.3"},
			},
			"",
			[]string{"detected GPU: NVIDIA GeForce RTX 4070 Ti", "installed and verified: /r/llama-server (variant cuda-13.3)", "not started"},
			[]string{"note:", "no GPU detected"},
		},
		{
			"no gpu",
			fakeInstaller{result: localmodel.RuntimeInstall{BinPath: "/r/llama-server", Variant: "cpu"}},
			"",
			[]string{"no GPU detected", "variant cpu"},
			[]string{"note:"},
		},
		{
			"already installed",
			fakeInstaller{result: localmodel.RuntimeInstall{BinPath: "/r/llama-server", Variant: "cpu", Cached: true}},
			"",
			[]string{"already installed: /r/llama-server (variant cpu)"},
			[]string{"not started"},
		},
		{
			"cpu build left on a machine with a gpu",
			fakeInstaller{
				status: localmodel.RuntimeStatus{GPU: "NVIDIA GeForce RTX 4070 Ti", Variant: "cpu", Variants: variants},
				result: localmodel.RuntimeInstall{BinPath: "/r/llama-server", Variant: "cpu"},
			},
			"cpu",
			[]string{"note: the cpu build runs on the CPU", "idle", "--variant", "cuda-13.3"},
			nil,
		},
		{
			"status unreadable",
			fakeInstaller{statusErr: errors.New("no pinned build"), result: localmodel.RuntimeInstall{BinPath: "/r/llama-server", Variant: "cpu"}},
			"",
			[]string{"installed and verified"},
			[]string{"detected GPU", "no GPU detected", "note:"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := installRuntime(t.Context(), &tc.f, localmodel.Runtime, tc.asked, &out); err != nil {
				t.Fatal(err)
			}
			if tc.f.asked != tc.asked {
				t.Errorf("the host was asked for variant %q, want %q", tc.f.asked, tc.asked)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, out.String())
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(out.String(), a) {
					t.Errorf("output has %q:\n%s", a, out.String())
				}
			}
		})
	}
}

func TestInstallRuntimeReportsAFailedInstall(t *testing.T) {
	f := fakeInstaller{installed: errors.New("digest mismatch")}
	err := installRuntime(t.Context(), &f, localmodel.Runtime, "", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "models install") || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("err = %v, want the install failure under the command name", err)
	}
}

func TestInstallRejectsStrayArguments(t *testing.T) {
	err := runRuntimeInstall([]string{"llama.cpp", "extra"}, filepath.Join(t.TempDir(), "data"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("err = %v; want an unexpected-argument refusal", err)
	}
	if err := runRuntimeInstall([]string{"--no-such-flag"}, filepath.Join(t.TempDir(), "data"), &bytes.Buffer{}); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}
