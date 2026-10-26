package localmodel_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/localmodel"
)

func TestRedactText(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		secrets []string
		want    string
	}{
		{"url reduced to host", "get https://h.test/a/b?sig=S failed", nil, "get https://h.test/... failed"},
		{"unparseable url", "bad http://%zz/x here", nil, "bad <url> here"},
		{"url with no host", "open file:///etc/passwd now", nil, "open <url> now"},
		{"secret replaced", "token abc123 leaked", []string{"abc123"}, "token <redacted> leaked"},
		{"empty secret ignored", "plain text", []string{""}, "plain text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := localmodel.RedactText(tc.in, tc.secrets...); got != tc.want {
				t.Fatalf("RedactText = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestFailRedactsEveryRememberedToken(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	h.RememberToken(4001, "tok-one-secret")
	h.RememberToken(4002, "tok-two-secret")

	err := h.Fail(errors.New("upstream said tok-one-secret and tok-two-secret at https://x.test/p?k=v"))
	for _, leak := range []string{"tok-one-secret", "tok-two-secret", "k=v"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("%q survived redaction: %v", leak, err)
		}
	}
	if again := h.Fail(err); again != err { //nolint:errorlint // identity, not equivalence, is what is asserted
		t.Fatal("an already redacted error was wrapped again")
	}
	if h.Fail(nil) != nil {
		t.Fatal("Fail(nil) is not nil")
	}
}

func TestHostedModelsAreNotRunnable(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cfg := f.config()
	hosted := localmodel.Model{ID: "api/hosted", Name: "Hosted", License: "proprietary", Kind: "api"}
	hosted.Source.Publisher = "vendor"
	hosted.Source.URL = "https://vendor.test/model"
	cfg.Catalog = append(cfg.Catalog, hosted)
	h, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	if _, err := h.Model("api/hosted"); err == nil || !strings.Contains(err.Error(), "hosted") {
		t.Fatalf("Model(hosted) = %v; want a hosted-model refusal", err)
	}
	if fit, err := h.Fit(t.Context(), "api/hosted"); err == nil || fit != localmodel.FitUnknown {
		t.Fatalf("Fit(hosted) = %q, %v; want unknown and an error", fit, err)
	}
	if _, err := h.Classify(""); err == nil {
		t.Fatal("Classify accepted an empty reference")
	}
}

func TestOpenRefusals(t *testing.T) {
	bare := func(f *fixture, cfg *localmodel.Config) localmodel.Config {
		return localmodel.Config{DataDir: cfg.DataDir, Catalog: []localmodel.Model{f.model}, MinContainment: "process"}
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, f *fixture, cfg *localmodel.Config)
	}{
		{"data dir under a file", func(t *testing.T, _ *fixture, cfg *localmodel.Config) {
			blocker := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(blocker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg.DataDir = filepath.Join(blocker, "data")
		}},
		{"release for another runtime", func(_ *testing.T, f *fixture, cfg *localmodel.Config) {
			r := f.release
			r.Runtime = "other"
			*cfg = localmodel.WithReleases(bare(f, cfg), r)
		}},
		{"release with no url", func(_ *testing.T, f *fixture, cfg *localmodel.Config) {
			r := f.release
			r.URL = ""
			*cfg = localmodel.WithReleases(bare(f, cfg), r)
		}},
		{"release with a non-numeric version", func(_ *testing.T, f *fixture, cfg *localmodel.Config) {
			r := f.release
			r.Version = "latest"
			*cfg = localmodel.WithReleases(bare(f, cfg), r)
		}},
		{"unknown containment", func(_ *testing.T, _ *fixture, cfg *localmodel.Config) { cfg.MinContainment = "strong" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			cfg := f.config()
			tc.mutate(t, f, &cfg)
			if h, err := localmodel.Open(cfg); err == nil {
				_ = h.Close()
				t.Fatal("Open accepted the configuration")
			}
		})
	}
}

func TestOpenAcceptsAZipRelease(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	r := f.release
	r.Zip = true
	cfg := localmodel.WithReleases(localmodel.Config{DataDir: f.dataDir, Catalog: []localmodel.Model{f.model}, MinContainment: "process"}, r)
	h, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
}

func TestEnsureRuntimeRefusals(t *testing.T) {
	t.Run("no build for this platform", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		r := f.release
		r.GOOS = "plan9"
		cfg := localmodel.WithReleases(localmodel.Config{DataDir: f.dataDir, Catalog: []localmodel.Model{f.model}, MinContainment: "process"}, r)
		h, err := localmodel.Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = h.Close() }()
		if _, err := h.EnsureRuntime(t.Context()); err == nil || !strings.Contains(err.Error(), "no pinned") {
			t.Fatalf("EnsureRuntime = %v; want no pinned build", err)
		}
	})
	t.Run("below a raised floor", func(t *testing.T) {
		for _, tc := range []struct{ advisory, want string }{
			{"GHSA-test-0001", "exposed to GHSA-test-0001"},
			{"", "older than the minimum this host raised"},
		} {
			f := newFixture(t, fixtureOpts{})
			h := f.open()
			if err := h.RaiseFloor("b100000", tc.advisory); err != nil {
				t.Fatal(err)
			}
			if got := h.Floor(); got != "100000" {
				t.Fatalf("Floor = %q after raising it", got)
			}
			if _, err := h.EnsureRuntime(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("EnsureRuntime = %v; want %q", err, tc.want)
			}
			if f.hits.Load() != 0 {
				t.Fatal("the origin was contacted before the floor was checked")
			}
		}
	})
	t.Run("archive that does not match its digest", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		f.release.SHA256 = strings.Repeat("0", 64)
		h := f.open()
		if _, err := h.EnsureRuntime(t.Context()); err == nil {
			t.Fatal("an archive with the wrong digest was installed")
		}
	})
	t.Run("a floor that is not a version", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		if err := f.open().RaiseFloor("none", ""); err == nil {
			t.Fatal("RaiseFloor accepted a version with no number")
		}
	})
}

func TestRuntimeBinaryChecks(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := localmodel.RecordRuntimeBinary(filepath.Join(dir, "missing"), bin+".sha256"); err == nil {
		t.Fatal("recorded the hash of a file that is not there")
	}
	if err := localmodel.VerifyRuntimeBinary(bin); err == nil {
		t.Fatal("verified a binary with no record")
	}
	if err := localmodel.RecordRuntimeBinary(bin, bin+".sha256"); err != nil {
		t.Fatal(err)
	}
	if err := localmodel.VerifyRuntimeBinary(bin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := localmodel.VerifyRuntimeBinary(bin); !errors.Is(err, localmodel.ErrDigestMismatch) {
		t.Fatalf("VerifyRuntimeBinary after a change = %v; want ErrDigestMismatch", err)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if err := localmodel.VerifyRuntimeBinary(bin); err == nil || errors.Is(err, localmodel.ErrDigestMismatch) {
		t.Fatalf("VerifyRuntimeBinary with the binary gone = %v; want a read failure", err)
	}
}

func TestHashFileFailures(t *testing.T) {
	dir := t.TempDir()
	if _, err := localmodel.HashFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("hashed a file that is not there")
	}
	// A directory opens on some platforms and fails on read; either way it has no hash.
	if _, err := localmodel.HashFile(dir); err == nil {
		t.Fatal("hashed a directory")
	}
}

func TestWeightChecks(t *testing.T) {
	dir := t.TempDir()
	body := ggufWithTemplate("")
	path := filepath.Join(dir, "w.gguf")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	good := localmodel.Weights{Path: path, SHA256: sum(body)}
	changed := localmodel.Weights{Path: path, SHA256: strings.Repeat("0", 64)}
	gone := localmodel.Weights{Path: filepath.Join(dir, "gone"), SHA256: sum(body)}
	onDir := localmodel.Weights{Path: dir, SHA256: sum(body)}

	if err := localmodel.VerifyWeights(good); err != nil {
		t.Fatal(err)
	}
	if err := localmodel.VerifiedTemplate(good); err != nil {
		t.Fatal(err)
	}
	if err := localmodel.VerifyWeights(changed); !errors.Is(err, localmodel.ErrDigestMismatch) {
		t.Fatalf("VerifyWeights(changed) = %v", err)
	}
	if err := localmodel.VerifiedTemplate(changed); !errors.Is(err, localmodel.ErrDigestMismatch) {
		t.Fatalf("VerifiedTemplate(changed) = %v", err)
	}
	if err := localmodel.VerifyWeights(gone); err == nil {
		t.Fatal("verified weights that are not there")
	}
	for name, w := range map[string]localmodel.Weights{"gone": gone, "directory": onDir} {
		if err := localmodel.VerifiedTemplate(w); err == nil {
			t.Fatalf("VerifiedTemplate(%s) succeeded", name)
		}
	}
	if runtime.GOOS != "windows" {
		if err := localmodel.VerifyWeights(onDir); err == nil {
			t.Fatal("verified a directory")
		}
	}
}

func TestSizeCeiling(t *testing.T) {
	for in, want := range map[int64]int64{-1: 0, 0: 0, 100: 105, 2000: 2100} {
		if got := localmodel.SizeCeiling(in); got != want {
			t.Fatalf("SizeCeiling(%d) = %d; want %d", in, got, want)
		}
	}
}

func TestSmallestServableNeedsAQuantization(t *testing.T) {
	if err := localmodel.SmallestServable(localmodel.Model{ID: "empty"}); err == nil || !strings.Contains(err.Error(), "no quantizations") {
		t.Fatalf("SmallestServable(no quants) = %v", err)
	}
}

func TestEnsureWeightsSelection(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	good := f.model.Quants[0]
	pickle := localmodel.Quant{Name: "PKL", Format: "pickle", SizeBytes: 10, URL: good.URL, Digest: good.Digest}
	safe := localmodel.Quant{Name: "ST", Format: "safetensors", SizeBytes: 5, URL: good.URL, Digest: good.Digest}
	multi := localmodel.Quant{Name: "SPLIT", Format: "gguf", SizeBytes: 7, Files: []localmodel.QuantFile{{URL: good.URL, Digest: good.Digest}, {URL: good.URL, Digest: good.Digest}}}
	noURL := localmodel.Quant{Name: "NOURL", Format: "gguf", SizeBytes: 9, Digest: good.Digest}

	cfg := f.config()
	cfg.Catalog = nil
	for id, qs := range map[string][]localmodel.Quant{
		"t/mixed":    {good, pickle, safe, multi, noURL},
		"t/nogguf":   {safe},
		"t/nopickle": {pickle},
	} {
		m := f.model
		m.ID, m.Quants = id, qs
		cfg.Catalog = append(cfg.Catalog, m)
	}
	var progress bytes.Buffer
	cfg.Progress = &progress
	h, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	tests := []struct {
		name, id, quant string
		wantErr         error
		wantText        string
	}{
		{"unknown model", "t/none", "", nil, "not in the catalog"},
		{"unknown quantization", "t/mixed", "Q9", nil, "no quantization"},
		{"pickle by name", "t/mixed", "PKL", localmodel.ErrNotServable, "code-executing"},
		{"safetensors by name", "t/mixed", "st", localmodel.ErrNotServable, "not a single GGUF"},
		{"multi-file by name", "t/mixed", "SPLIT", localmodel.ErrNotServable, "not a single GGUF"},
		{"no direct download", "t/mixed", "NOURL", localmodel.ErrNotServable, "no direct download"},
		{"only safetensors, unnamed", "t/nogguf", "", localmodel.ErrNotServable, "not a single GGUF"},
		{"only pickle, unnamed", "t/nopickle", "", localmodel.ErrNotServable, "code-executing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.EnsureWeights(t.Context(), tc.id, tc.quant)
			if err == nil || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("EnsureWeights = %v; want %q", err, tc.wantText)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("EnsureWeights = %v; want it to match %v", err, tc.wantErr)
			}
		})
	}

	w, err := h.EnsureWeights(t.Context(), "t/mixed", "q4_k_m")
	if err != nil || w.Quant != "Q4_K_M" {
		t.Fatalf("EnsureWeights by a differently cased name = %+v, %v", w, err)
	}
	if !strings.Contains(progress.String(), "fetching t/mixed") || strings.Contains(progress.String(), "SECRET-SIGNATURE") {
		t.Fatalf("progress = %q; want a line that names the model and no URL", progress.String())
	}
}

// The runtime binary is hashed again just before launch, so a build swapped after install
// is refused and never started.
func TestRuntimeSwappedBeforeLaunchIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	spy := h.SpyOnLaunches()
	rt, err := h.EnsureRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h.BeforeLaunchCheck(func(int) {
		if err := os.WriteFile(rt.BinPath, []byte("not the build that was installed"), 0o700); err != nil {
			t.Error(err)
		}
	})
	if _, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{}); !errors.Is(err, localmodel.ErrDigestMismatch) {
		t.Fatalf("Serve = %v; want ErrDigestMismatch", err)
	}
	if n := len(spy.Specs()); n != 0 {
		t.Fatalf("the swapped runtime was launched (%d launches)", n)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/next" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer srv.Close()
	u := strings.Replace(srv.URL, "localhost", "127.0.0.1", 1)

	resp, err := h.Client().Get(u)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || followed.Load() {
		t.Fatalf("status %d, followed = %v; want the redirect returned unfollowed", resp.StatusCode, followed.Load())
	}
}
