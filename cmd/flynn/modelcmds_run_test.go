package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
	"github.com/ionalpha/flynn/internal/inference/serve"
	"github.com/ionalpha/flynn/sandbox"
)

// localCatalogModelID is a catalog model that is local and carries a pinned direct
// download, so it takes the whole gate-and-serve path with the runtime and the fetch
// stubbed out.
const localCatalogModelID = "qwen2.5:0.5b-instruct"

// noDownloadModelID is a local catalog model with no pinned direct download, the case the
// serve and provision paths refuse rather than guess at.
const noDownloadModelID = "ollama:qwen2.5-coder:1.5b"

// stubModelServer is an OpenAI-compatible server bound to a chosen loopback port, standing
// in for the local runtime a served model is reached at. It answers every completion with a
// fixed message, or with a failure when broken is set, and counts the requests it saw.
type stubModelServer struct {
	port  int
	calls atomic.Int64
}

// newStubModelServer binds a loopback listener, so the port it answers on is the port the
// runner hands the serve manager and the endpoint the model client then dials.
func newStubModelServer(t *testing.T, reply string, broken bool) *stubModelServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind loopback: %v", err)
	}
	s := &stubModelServer{port: ln.Addr().(*net.TCPAddr).Port}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.calls.Add(1)
		if broken {
			http.Error(w, "the runtime fell over", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, reply,
		))
	}))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return s
}

// newGatedRunner builds a localRunner that takes the real admission path (a real sandbox, a
// real provenance ledger, the real catalog) but stubs everything that would touch the network
// or a real runtime: the runtime binary, the weights, and the server process. The served
// endpoint points at port, so a stub model server bound there is what the run then talks to.
func newGatedRunner(t *testing.T, port int, out io.Writer) *localRunner {
	t.Helper()
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	sb, err := sandbox.NewLocal(runDir)
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	weights := writeMinimalGGUF(t, map[string]string{"general.architecture": "qwen2"})
	mgr := serve.NewManager(
		&fakeServeLauncher{},
		func(context.Context, string) error { return nil }, // the stub server is always healthy
		func(int) error { return nil },
		serve.NewRegistry(runDir),
	)
	return &localRunner{
		dataDir:       dataDir,
		out:           out,
		ensureRuntime: func(context.Context, string) (string, error) { return "/runtimes/llama-server", nil },
		ensureWeights: func(context.Context, catalog.ModelSpec, catalog.Quant) (string, error) { return weights, nil },
		manager:       mgr,
		freePort:      func() (int, error) { return port, nil },
		sb:            sb,
		ledger:        modelsource.NewLedger(filepath.Join(dataDir, "models")),
	}
}

// TestModelRunServesThenAnswers walks the command a user runs: the model is provisioned and
// served inside the sandbox, the prompt reaches it over its loopback endpoint, and the
// answer is printed. A second run reuses the server that is already up rather than starting
// a second one.
func TestModelRunServesThenAnswers(t *testing.T) {
	stub := newStubModelServer(t, "42", false)
	var out bytes.Buffer
	r := newGatedRunner(t, stub.port, &out)

	if err := modelRun(context.Background(), r, consentPrompt, localCatalogModelID, "what is six times seven", &out); err != nil {
		t.Fatalf("modelRun: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, fmt.Sprintf("serving %s at http://127.0.0.1:%d/v1", localCatalogModelID, stub.port)) {
		t.Errorf("the serve line must name the model and its loopback endpoint, got:\n%s", got)
	}
	if !strings.Contains(got, "confined to the sandbox") {
		t.Errorf("a served model must be reported as confined, got:\n%s", got)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("the model's answer must be printed, got:\n%s", got)
	}
	if stub.calls.Load() != 1 {
		t.Errorf("the model was asked %d times, want exactly one prompt", stub.calls.Load())
	}

	var second bytes.Buffer
	r.out = &second
	if err := modelRun(context.Background(), r, consentPrompt, localCatalogModelID, "", &second); err != nil {
		t.Fatalf("second modelRun: %v", err)
	}
	if !strings.Contains(second.String(), "reusing the running server") {
		t.Errorf("an already-served model must be reused, got:\n%s", second.String())
	}
	// With no prompt the run leaves the server up and says how to use and stop it.
	if !strings.Contains(second.String(), "the model is ready.") ||
		!strings.Contains(second.String(), "flynn models stop "+localCatalogModelID) {
		t.Errorf("a promptless run must name the next commands, got:\n%s", second.String())
	}
	if stub.calls.Load() != 1 {
		t.Errorf("a promptless run must not prompt the model, calls = %d", stub.calls.Load())
	}
}

// TestModelRunReportsAModelFailure checks a runtime that answers with an error is reported as
// a failure to answer, not as a silent empty response.
func TestModelRunReportsAModelFailure(t *testing.T) {
	stub := newStubModelServer(t, "", true)
	var out bytes.Buffer
	r := newGatedRunner(t, stub.port, &out)

	err := modelRun(context.Background(), r, consentPrompt, localCatalogModelID, "hello", &out)
	if err == nil || !strings.Contains(err.Error(), "the model failed to answer") {
		t.Fatalf("error = %v, want a failed-to-answer error", err)
	}
}

// TestModelRunRefusesAnUnservableModel checks the two refusals a run can hit: a reference
// that is neither a catalog id nor a recognized weight format, and a catalog model with no
// pinned download to serve.
func TestModelRunRefusesAnUnservableModel(t *testing.T) {
	cases := map[string]string{
		"no-such-model":   "unrecognized weight format",
		noDownloadModelID: "no pinned direct download",
	}
	for id, want := range cases {
		var out bytes.Buffer
		r := newGatedRunner(t, 1, &out)
		err := modelRun(context.Background(), r, consentPrompt, id, "", &out)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("modelRun(%q) = %v, want an error naming %q", id, err, want)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "models run: ") {
			t.Errorf("the failure must be reported as a models run error, got %v", err)
		}
	}
}

// TestProvisionAndSelectRecordsTheDefault checks `models use` on a catalog model: the runtime
// and weights are provisioned, and only then is the model recorded as the default, so a
// selection that could not be provisioned is never left as the default.
func TestProvisionAndSelectRecordsTheDefault(t *testing.T) {
	var out bytes.Buffer
	r := newGatedRunner(t, 1, &out)
	m, err := findLocalModel(localCatalogModelID)
	if err != nil {
		t.Fatalf("findLocalModel: %v", err)
	}

	var provisioned []string
	r.ensureRuntime = func(_ context.Context, name string) (string, error) {
		provisioned = append(provisioned, name)
		return "/runtimes/llama-server", nil
	}
	if err := provisionAndSelect(context.Background(), r, m, localCatalogModelID, &out); err != nil {
		t.Fatalf("provisionAndSelect: %v", err)
	}
	if len(provisioned) != 1 || provisioned[0] != selfProvisionedRuntime {
		t.Errorf("provisioned = %v, want the self-provisioned runtime", provisioned)
	}
	if got, ok := readActiveModel(r.dataDir); !ok || got != localCatalogModelID {
		t.Errorf("active model = %q (recorded %v), want the selected model", got, ok)
	}
	if !strings.Contains(out.String(), "is provisioned and set as the default model") {
		t.Errorf("out = %q", out.String())
	}
}

// TestProvisionAndSelectRefusesWhatItCannotProvision checks a failure at any provisioning
// step leaves no default recorded: a model that cannot be fetched must not become the model a
// later `flynn goal` starts.
func TestProvisionAndSelectRefusesWhatItCannotProvision(t *testing.T) {
	m, err := findLocalModel(localCatalogModelID)
	if err != nil {
		t.Fatalf("findLocalModel: %v", err)
	}
	noDownload, err := findLocalModel(noDownloadModelID)
	if err != nil {
		t.Fatalf("findLocalModel: %v", err)
	}

	cases := []struct {
		name    string
		model   catalog.ModelSpec
		id      string
		prepare func(*localRunner)
		want    string
	}{
		{
			name: "no pinned download", model: noDownload, id: noDownloadModelID,
			prepare: func(*localRunner) {}, want: "no pinned direct download",
		},
		{
			name: "runtime refused", model: m, id: localCatalogModelID,
			prepare: func(r *localRunner) {
				r.ensureRuntime = func(context.Context, string) (string, error) {
					return "", errors.New("gate refused a vulnerable build")
				}
			},
			want: "gate refused",
		},
		{
			name: "weights refused", model: m, id: localCatalogModelID,
			prepare: func(r *localRunner) {
				r.ensureWeights = func(context.Context, catalog.ModelSpec, catalog.Quant) (string, error) {
					return "", errors.New("digest mismatch")
				}
			},
			want: "digest mismatch",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			r := newGatedRunner(t, 1, &out)
			c.prepare(r)
			err := provisionAndSelect(context.Background(), r, c.model, c.id, &out)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to name %q", err, c.want)
			}
			if got, ok := readActiveModel(r.dataDir); ok {
				t.Errorf("a failed provisioning recorded %q as the default", got)
			}
		})
	}
}

// TestModelProbeMeasuresAndRecords drives `models probe` against a served stub: the battery
// runs, the scores and the measured context are reported, and the profile is written so a
// later run is scaffolded from the measurement rather than from an assumption.
func TestModelProbeMeasuresAndRecords(t *testing.T) {
	stub := newStubModelServer(t, "ok", false)
	var out bytes.Buffer
	r := newGatedRunner(t, stub.port, &out)

	if err := modelProbe(context.Background(), r, consentPrompt, localCatalogModelID, &out); err != nil {
		t.Fatalf("modelProbe: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"probing " + localCatalogModelID,
		"reliability for " + localCatalogModelID,
		"tool calls:", "schema:", "instructions:", "context:",
		"recorded; future runs of this model are scaffolded from this measurement.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("probe output is missing %q, got:\n%s", want, got)
		}
	}
	if stub.calls.Load() == 0 {
		t.Error("the battery must actually reach the served model")
	}

	// The measurement is durable: the plan for the model is now built from the recorded
	// profile rather than from the unmeasured default.
	if src := localProfileSource(context.Background(), r.dataDir); src == nil {
		t.Fatal("the recorded profile must be readable back")
	}
	if _, ok := localProfileSource(context.Background(), r.dataDir).Profile(localCatalogModelID); !ok {
		t.Error("probe must record a profile for the model it measured")
	}
}

// TestModelProbeRefusesWhatItCannotServe checks the probe reports its own command name when
// the model cannot be served at all, rather than a bare gate error.
func TestModelProbeRefusesWhatItCannotServe(t *testing.T) {
	var out bytes.Buffer
	r := newGatedRunner(t, 1, &out)
	err := modelProbe(context.Background(), r, consentPrompt, "no-such-model", &out)
	if err == nil || !strings.HasPrefix(err.Error(), "models probe: ") {
		t.Fatalf("error = %v, want a models probe error", err)
	}
}

// TestRunModelStatusListsALiveServer checks the running view: a recorded server that still
// answers its health probe is listed with its runtime, endpoint, and pid, so a user can see
// what is up and what to stop.
func TestRunModelStatusListsALiveServer(t *testing.T) {
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// The status rendering is what is under test, so its input (a server the manager confirms
	// live) is supplied directly: a recorded server plus a probe scripted to report it healthy.
	// Standing up a real endpoint and probing it over loopback within a wall-clock timeout is a
	// race on a loaded host, and it exercises the prober, not the code this test is about.
	rec := serve.Record{
		ModelID: localCatalogModelID,
		PID:     4321,
		Port:    57963,
		BaseURL: "http://127.0.0.1:57963/v1",
		Runtime: selfProvisionedRuntime,
	}
	reg := serve.NewRegistry(runDir)
	if err := reg.Put(rec); err != nil {
		t.Fatalf("record a server: %v", err)
	}
	manager := serve.NewManager(
		&fakeServeLauncher{},
		func(context.Context, string) error { return nil }, // the recorded server answers its health probe
		func(int) error { return nil },
		reg,
	)

	var out bytes.Buffer
	if err := writeModelStatus(context.Background(), manager, dataDir, &out); err != nil {
		t.Fatalf("writeModelStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{"MODEL", "RUNTIME", "ENDPOINT", "PID", localCatalogModelID, rec.BaseURL, "4321"} {
		if !strings.Contains(got, want) {
			t.Errorf("the status table is missing %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "no local model servers are running") {
		t.Errorf("a live server must not be reported as idle, got:\n%s", got)
	}
}
