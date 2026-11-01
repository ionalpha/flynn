package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/provider"
)

const (
	gb        = int64(1_000_000_000)
	goodScore = 0.95
	weakScore = 0.5
)

func initModel(id string, sizeGB int64, mut ...func(*catalog.ModelSpec)) catalog.ModelSpec {
	m := catalog.ModelSpec{
		ID: id, Name: id, Kind: catalog.KindLocal, Trust: catalog.TrustBlessed,
		ParamsB: float64(sizeGB), ContextTokens: 8192,
		Capabilities: catalog.Capabilities{Tools: true},
		Quants: []catalog.Quant{{
			Name: "Q4_K_M", Format: catalog.FormatGGUF, SizeBytes: sizeGB * gb,
			URL: "https://example.com/" + id + ".gguf", Digest: "sha256:" + strings.Repeat("ab", 32),
		}},
	}
	for _, f := range mut {
		f(&m)
	}
	return m
}

// twelveGB is the 12 GB GPU the live test runs on, with room to spill into RAM.
func twelveGB() fitMachine {
	return fitMachine{Machine: catalog.Machine{VRAMFreeBytes: 11 * gb, RAMBytes: 24 * gb}, description: "Test GPU, 11 GB free of 12 GB VRAM (detected)"}
}

func noMachine() fitMachine { return fitMachine{description: "nothing detected"} }

// initFake is a scripted machine: what the wizard finds, and a record of what it did.
type initFake struct {
	machine  fitMachine
	models   []catalog.ModelSpec
	servers  []provider.LocalModel
	named    []string
	rt       initRuntimeState
	rtErr    error
	score    float64
	noScore  bool
	probeErr error
	fetchErr error

	calls []string
	used  string
}

// syncBuf is an output sink safe to write from the prompt's reader goroutine while the
// test reads it back.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (f *initFake) env(in io.Reader, yes bool) (initEnv, *syncBuf) {
	out := &syncBuf{}
	f.score = firstNonZero(f.score, goodScore)
	scored := map[string]bool{}
	return initEnv{
		in: bufio.NewReader(in), out: out, yes: yes, dataDir: "unused",
		machine: func(float64, float64) fitMachine { return f.machine },
		catalog: func() (catalog.Catalog, error) { return catalog.Catalog{Models: f.models}, nil },
		profiles: func(context.Context) harness.ProfileSource {
			if f.noScore {
				return harness.StaticProfiles{}
			}
			p := harness.StaticProfiles{}
			for id := range scored {
				p[id] = harness.ModelProfile{ToolCallReliability: f.score, StructuredOutput: f.score, InstructionFollowing: f.score}
			}
			return p
		},
		discover: func(context.Context) []provider.LocalModel { return f.servers },
		servers:  func() ([]string, error) { return f.named, nil },
		runtime:  func(context.Context) (initRuntimeState, error) { return f.rt, f.rtErr },
		install: func(context.Context) error {
			f.calls = append(f.calls, "install")
			return nil
		},
		fetch: func(_ context.Context, c initChoice) error {
			f.calls = append(f.calls, "fetch "+c.ID)
			return f.fetchErr
		},
		probe: func(_ context.Context, c initChoice) error {
			f.calls = append(f.calls, "probe "+c.ID)
			if f.probeErr == nil {
				scored[c.ID] = true
			}
			return f.probeErr
		},
		use: func(_ context.Context, id string) error {
			f.calls = append(f.calls, "use "+id)
			f.used = id
			return nil
		},
	}, out
}

func firstNonZero(a, b float64) float64 {
	if a != 0 {
		return a
	}
	return b
}

func installed() initRuntimeState {
	return initRuntimeState{Installed: true, Variant: "cuda-13.3"}
}

func TestInitWizard(t *testing.T) {
	small, mid := initModel("small-3b", 3), initModel("mid-7b", 7)
	ollama := provider.LocalModel{Server: "ollama", ID: "qwen3:8b", Params: "8B", Quant: "Q4_K_M", Tools: true}

	tests := []struct {
		name      string
		fake      initFake
		input     string
		yes       bool
		model     string
		wantErr   string // substring; "stopped" means errInitStopped
		wantCalls []string
		wantOut   []string
		denyOut   []string
	}{
		{
			name:      "yes takes the first recommendation and installs the missing runtime",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small, mid}, rt: initRuntimeState{Variant: "cuda-13.3", DownloadBytes: 200_000_000}},
			yes:       true,
			wantCalls: []string{"install", "fetch mid-7b", "probe mid-7b", "use mid-7b"},
			wantOut:   []string{"Test GPU", "1) mid-7b", "2) small-3b", "Downloading the llama.cpp runtime (cuda-13.3 build): 200", "Downloading mid-7b (Q4_K_M): 7", `flynn goal "`},
		},
		{
			name:      "an installed runtime is not offered again",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed()},
			yes:       true,
			wantCalls: []string{"fetch small-3b", "probe small-3b", "use small-3b"},
			denyOut:   []string{"Downloading the llama.cpp"},
		},
		{
			name:      "enter accepts the choice and both downloads",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: initRuntimeState{Variant: "cpu"}},
			input:     "\n\n\n",
			wantCalls: []string{"install", "fetch small-3b", "probe small-3b", "use small-3b"},
			wantOut:   []string{"Continue? [Y/n]"},
		},
		{
			name:      "a number picks the second model",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small, mid}, rt: installed()},
			input:     "2\ny\n",
			wantCalls: []string{"fetch small-3b", "probe small-3b", "use small-3b"},
		},
		{
			name:      "an id typed in full picks that model",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small, mid}, rt: installed()},
			input:     "small-3b\n\n",
			wantCalls: []string{"fetch small-3b", "probe small-3b", "use small-3b"},
		},
		{
			name:      "a wrong answer is asked again",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed()},
			input:     "9\nbanana\n1\n\n",
			wantCalls: []string{"fetch small-3b", "probe small-3b", "use small-3b"},
			wantOut:   []string{`"9" is not one of the options`, `"banana" is not one of the options`},
		},
		{
			name:    "declining the model download changes nothing",
			fake:    initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed()},
			input:   "1\nn\n",
			wantErr: "stopped",
			wantOut: []string{"Download declined. The default model is unchanged"},
		},
		{
			name:    "declining the runtime install changes nothing and fetches nothing",
			fake:    initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: initRuntimeState{Variant: "cpu"}},
			input:   "1\nno\n",
			wantErr: "stopped",
		},
		{
			name:    "quitting at the menu changes nothing",
			fake:    initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed()},
			input:   "q\n",
			wantErr: "stopped",
			wantOut: []string{"No model chosen"},
		},
		{
			name:    "end of input at a prompt is an error, not a default",
			fake:    initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed()},
			input:   "",
			wantErr: "read input",
		},
		{
			name:      "a server model is offered after the downloads and needs none",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, servers: []provider.LocalModel{ollama}, rt: installed()},
			input:     "2\n",
			wantCalls: []string{"probe ollama:qwen3:8b", "use ollama:qwen3:8b"},
			wantOut:   []string{"ollama (1 models)", "Already running on this machine", "2) ollama:qwen3:8b  8B Q4_K_M, already on this machine, no download"},
		},
		{
			name:      "with no GPU detected only the running server is offered",
			fake:      initFake{machine: noMachine(), models: []catalog.ModelSpec{small}, servers: []provider.LocalModel{ollama}, rt: installed()},
			yes:       true,
			wantCalls: []string{"probe ollama:qwen3:8b", "use ollama:qwen3:8b"},
			wantOut:   []string{"could not detect GPU or system memory"},
		},
		{
			name:    "nothing fits and nothing runs",
			fake:    initFake{machine: twelveGB(), models: []catalog.ModelSpec{initModel("huge-70b", 70)}, rt: installed()},
			yes:     true,
			wantErr: "stopped",
			wantOut: []string{"Nothing to set up automatically", "flynn models --local --fit"},
		},
		{
			name:    "an unreadable runtime is stated and blocks a download",
			fake:    initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rtErr: errors.New("no build for this OS")},
			yes:     true,
			wantErr: "cannot be installed here",
			wantOut: []string{"llama.cpp cannot be installed here (no build for this OS)"},
		},
		{
			name:    "a corrupt runtime is not used",
			fake:    initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: initRuntimeState{Installed: true, Variant: "cpu", Problem: "digest mismatch"}},
			yes:     true,
			wantErr: "integrity check",
		},
		{
			name:      "a failed download leaves no default",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed(), fetchErr: errors.New("connection reset")},
			yes:       true,
			wantErr:   "connection reset",
			wantCalls: []string{"fetch small-3b"},
		},
		{
			name:      "a failed probe leaves no default",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed(), probeErr: errors.New("model did not load")},
			yes:       true,
			wantErr:   "did not complete the probe",
			wantCalls: []string{"fetch small-3b", "probe small-3b"},
		},
		{
			name:      "a weak model is not made the default under --yes",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed(), score: weakScore},
			yes:       true,
			wantErr:   "stopped",
			wantCalls: []string{"fetch small-3b", "probe small-3b"},
			wantOut:   []string{"scored 50% on tool calls", "`flynn models use small-3b`"},
		},
		{
			name:      "a weak model can be accepted by name at the prompt",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed(), score: weakScore},
			input:     "1\ny\ny\n",
			wantCalls: []string{"fetch small-3b", "probe small-3b", "use small-3b"},
		},
		{
			name:      "a weak model is refused by default at the prompt",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed(), score: weakScore},
			input:     "1\ny\n\n",
			wantErr:   "stopped",
			wantCalls: []string{"fetch small-3b", "probe small-3b"},
			wantOut:   []string{"Not set as the default"},
		},
		{
			name:      "a probe that recorded nothing counts as unproven",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, rt: installed(), noScore: true},
			yes:       true,
			wantErr:   "stopped",
			wantCalls: []string{"fetch small-3b", "probe small-3b"},
			wantOut:   []string{"recorded no scores"},
		},
		{
			name:      "--model names a catalog model that fits",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small, mid}, rt: installed()},
			yes:       true,
			model:     "small-3b",
			wantCalls: []string{"fetch small-3b", "probe small-3b", "use small-3b"},
		},
		{
			name:    "--model refuses a catalog model that does not fit",
			fake:    initFake{machine: twelveGB(), models: []catalog.ModelSpec{initModel("huge-70b", 70)}, rt: installed()},
			yes:     true,
			model:   "huge-70b",
			wantErr: "does not fit this machine",
		},
		{
			name: "--model refuses a catalog model with no pinned download",
			fake: initFake{machine: twelveGB(), models: []catalog.ModelSpec{initModel("loose-3b", 3, func(m *catalog.ModelSpec) {
				m.Quants[0].URL = ""
			})}, rt: installed()},
			yes:     true,
			model:   "loose-3b",
			wantErr: "with a pinned download",
		},
		{
			name:      "--model takes a model a running server holds",
			fake:      initFake{machine: twelveGB(), models: []catalog.ModelSpec{small}, servers: []provider.LocalModel{ollama}, rt: installed()},
			yes:       true,
			model:     "ollama:qwen3:8b",
			wantCalls: []string{"probe ollama:qwen3:8b", "use ollama:qwen3:8b"},
		},
		{
			name:      "--model takes a model on a configured server",
			fake:      initFake{machine: twelveGB(), named: []string{"box"}, rt: installed()},
			yes:       true,
			model:     "box:llama3",
			wantCalls: []string{"probe box:llama3", "use box:llama3"},
		},
		{
			name:    "--model refuses a hosted provider",
			fake:    initFake{machine: twelveGB(), rt: installed()},
			yes:     true,
			model:   "anthropic:claude-sonnet-4",
			wantErr: "is a hosted provider",
		},
		{
			name:    "--model refuses a server nobody configured",
			fake:    initFake{machine: twelveGB(), rt: installed()},
			yes:     true,
			model:   "nowhere:llama3",
			wantErr: "no server this install knows",
		},
		{
			name:    "--model refuses a bare word that is not in the catalog",
			fake:    initFake{machine: twelveGB(), rt: installed()},
			yes:     true,
			model:   "mystery",
			wantErr: "not in the catalog",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, out := tc.fake.env(strings.NewReader(tc.input), tc.yes)
			err := env.run(context.Background(), tc.model)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v\n%s", err, out)
			case tc.wantErr == "stopped" && !errors.Is(err, errInitStopped):
				t.Fatalf("err = %v, want the silent stop\n%s", err, out)
			case tc.wantErr != "" && tc.wantErr != "stopped" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want it to contain %q\n%s", err, tc.wantErr, out)
			}
			if got, want := strings.Join(tc.fake.calls, "; "), strings.Join(tc.wantCalls, "; "); got != want {
				t.Errorf("calls = [%s], want [%s]", got, want)
			}
			if tc.wantErr != "" && tc.fake.used != "" {
				t.Errorf("default was set to %q on a run that ended in %v", tc.fake.used, err)
			}
			for _, s := range tc.wantOut {
				if !strings.Contains(out.String(), s) {
					t.Errorf("output missing %q:\n%s", s, out)
				}
			}
			for _, s := range tc.denyOut {
				if strings.Contains(out.String(), s) {
					t.Errorf("output has %q:\n%s", s, out)
				}
			}
		})
	}
}

// TestInitInterruptLeavesDefaultUnset covers Ctrl-C while a prompt is waiting on the
// terminal: the wizard must return without a line of input, say nothing was changed, and
// never reach the step that writes the default.
func TestInitInterruptLeavesDefaultUnset(t *testing.T) {
	fake := &initFake{machine: twelveGB(), models: []catalog.ModelSpec{initModel("small-3b", 3)}, rt: installed()}
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	env, out := fake.env(pr, false)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.run(ctx, "") }()
	time.AfterFunc(50*time.Millisecond, cancel)

	select {
	case err := <-done:
		if !errors.Is(err, errInitStopped) {
			t.Fatalf("err = %v, want the silent stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wizard stayed blocked on the prompt after the interrupt")
	}
	if !strings.Contains(out.String(), "Interrupted. The default model is unchanged") {
		t.Errorf("output did not say nothing changed:\n%s", out)
	}
	if len(fake.calls) != 0 || fake.used != "" {
		t.Errorf("steps ran after the interrupt: %v", fake.calls)
	}
}

func TestInitCatalogErrorIsReported(t *testing.T) {
	fake := &initFake{machine: twelveGB(), rt: installed()}
	env, _ := fake.env(strings.NewReader(""), true)
	env.catalog = func() (catalog.Catalog, error) { return catalog.Catalog{}, errors.New("overlay unreadable") }
	if err := env.run(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "overlay unreadable") {
		t.Fatalf("err = %v, want the catalog error", err)
	}
}

func TestInitRecommend(t *testing.T) {
	gpu := twelveGB()
	cpuOnly := fitMachine{Machine: catalog.Machine{RAMBytes: 24 * gb}, description: "no GPU"}
	spillover := fitMachine{Machine: catalog.Machine{VRAMFreeBytes: 4 * gb, RAMBytes: 24 * gb}, description: "small GPU"}

	tests := []struct {
		name   string
		mc     fitMachine
		models []catalog.ModelSpec
		want   []string
	}{
		{"largest first, at most three", gpu, []catalog.ModelSpec{initModel("a-2b", 2), initModel("b-3b", 3), initModel("c-4b", 4), initModel("d-5b", 5)}, []string{"d-5b", "c-4b", "b-3b"}},
		{"unverified entries are left out", gpu, []catalog.ModelSpec{initModel("mine-3b", 3, func(m *catalog.ModelSpec) { m.Trust = catalog.TrustLocal }), initModel("a-2b", 2)}, []string{"a-2b"}},
		{"a model without tool calling is left out", gpu, []catalog.ModelSpec{initModel("chat-3b", 3, func(m *catalog.ModelSpec) { m.Capabilities.Tools = false })}, nil},
		{"a multi-file model is left out", gpu, []catalog.ModelSpec{initModel("split-3b", 3, func(m *catalog.ModelSpec) { m.Quants[0].Files = []catalog.QuantFile{{Name: "a"}, {Name: "b"}} })}, nil},
		{"a non-GGUF model is left out", gpu, []catalog.ModelSpec{initModel("st-3b", 3, func(m *catalog.ModelSpec) { m.Quants[0].Format = catalog.FormatSafetensors })}, nil},
		{"a model without a size is left out", gpu, []catalog.ModelSpec{initModel("blank-3b", 3, func(m *catalog.ModelSpec) { m.Quants[0].SizeBytes = 0 })}, nil},
		{"a hosted model is left out", gpu, []catalog.ModelSpec{initModel("api-big", 3, func(m *catalog.ModelSpec) { m.Kind = catalog.KindAPI })}, nil},
		{"a model that only runs from RAM beside a GPU is left out", spillover, []catalog.ModelSpec{initModel("slow-9b", 9)}, nil},
		{"a machine with nothing detected recommends nothing", noMachine(), []catalog.ModelSpec{initModel("a-2b", 2)}, nil},
		{"a machine with no GPU still gets models that fit in RAM", cpuOnly, []catalog.ModelSpec{initModel("a-2b", 2)}, []string{"a-2b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &initFake{machine: tc.mc}
			env, _ := fake.env(strings.NewReader(""), true)
			got := env.recommend(catalog.Catalog{Models: tc.models}, tc.mc, harness.StaticProfiles{})
			var ids []string
			for _, c := range got {
				ids = append(ids, c.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Errorf("recommended %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestInitAdoptions(t *testing.T) {
	found := []provider.LocalModel{
		{Server: "ollama", ID: "chat-only:7b"},
		{Server: "ollama", ID: "tools:8b", Tools: true},
		{Server: "lmstudio", ID: "unknown-caps"},
	}
	got := adoptions(found)
	var ids []string
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if want := "ollama:tools:8b,lmstudio:unknown-caps"; strings.Join(ids, ",") != want {
		t.Fatalf("adoptions = %v, want %s", ids, want)
	}
	if !strings.Contains(got[1].Detail, "tool support not reported") {
		t.Errorf("an unreported tool capability is not said to be unreported: %q", got[1].Detail)
	}

	many := make([]provider.LocalModel, 9)
	for i := range many {
		many[i] = provider.LocalModel{Server: "ollama", ID: string(rune('a'+i)) + ":1b", Tools: true}
	}
	if n := len(adoptions(many)); n != initAdoptions {
		t.Errorf("listed %d server models, want at most %d", n, initAdoptions)
	}
}

func TestServerModelSpec(t *testing.T) {
	tests := []struct {
		in         provider.LocalModel
		wantParams float64
		wantQuant  string
	}{
		{provider.LocalModel{Server: "ollama", ID: "m", Params: "20.9B", Quant: "MXFP4"}, 20.9, "MXFP4"},
		{provider.LocalModel{Server: "ollama", ID: "m", Params: "7b"}, 7, ""},
		{provider.LocalModel{Server: "lmstudio", ID: "m"}, 0, ""},
		{provider.LocalModel{Server: "ollama", ID: "m", Params: "lots"}, 0, ""},
	}
	for _, tc := range tests {
		m := serverModelSpec(tc.in)
		if m.ID != tc.in.Spec() || !m.Local() || m.ParamsB != tc.wantParams {
			t.Errorf("serverModelSpec(%+v) = id %q local %v params %v", tc.in, m.ID, m.Local(), m.ParamsB)
		}
		q, ok := m.SmallestQuant()
		if tc.wantQuant == "" && ok || tc.wantQuant != "" && q.Name != tc.wantQuant {
			t.Errorf("serverModelSpec(%+v) quant = %q (%v), want %q", tc.in, q.Name, ok, tc.wantQuant)
		}
	}
}

func TestRunInitRefusals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"without --local", nil, "flynn init --local"},
		{"an unexpected argument", []string{"--local", "extra"}, `unexpected argument "extra"`},
		{"no terminal and no --yes", []string{"--local"}, "re-run with --yes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prev := initHasTerminal
			initHasTerminal = func() bool { return false }
			defer func() { initHasTerminal = prev }()
			err := runInit(tc.args, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runInit(%v) = %v, want it to contain %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestYesAnswer(t *testing.T) {
	for _, tc := range []struct {
		in   string
		def  bool
		want bool
	}{{"", true, true}, {"", false, false}, {"y", false, true}, {"YES", false, true}, {"n", true, false}, {"maybe", true, false}} {
		if got := yesAnswer(tc.in, tc.def); got != tc.want {
			t.Errorf("yesAnswer(%q, %v) = %v, want %v", tc.in, tc.def, got, tc.want)
		}
	}
}

// TestLiveInitEnvReadsThisInstall runs the wizard's detection steps against the real
// functions on an empty data directory, so the wiring to the models commands is exercised
// without a download or a model server.
func TestLiveInitEnvReadsThisInstall(t *testing.T) {
	env := liveInitEnv(t.TempDir(), strings.NewReader(""), io.Discard, true)
	ctx := context.Background()
	if _, err := env.catalog(); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if env.profiles(ctx) == nil {
		t.Error("no profile source")
	}
	if names, err := env.servers(); err != nil || len(names) != 0 {
		t.Errorf("servers = %v, %v; want none on an empty data directory", names, err)
	}
	if got := env.discover(ctx); len(got) != 0 {
		t.Errorf("discovery found %v; the package tests stub it to find nothing", got)
	}
	if _, err := env.runtime(ctx); err != nil {
		t.Logf("runtime status unavailable on this platform: %v", err)
	}
}
