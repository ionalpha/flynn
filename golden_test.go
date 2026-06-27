package agent

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/mission"
)

// Golden missions guard end-to-end behaviour. Each scenario drives the real agent
// assembly (the same path Goal takes) with a scripted model, so the run needs no
// network and no API key, and records the mission's conversational event stream,
// final answer, and the files it materialized. The recorded spine is committed to
// testdata/ and re-folded on every run: any divergence fails the build, which is
// the live gate behind the deterministic-replay guarantee. Regenerate after an
// intended behaviour change with `go test -run TestGoldenMissions -update`.
//
// The test also asserts replay determinism directly: each scenario runs twice and
// the two recordings must be byte-identical. Because CI runs this on Linux, macOS,
// and Windows, a green build is also cross-platform replay equivalence, so a
// map-order, path-separator, or float-formatting leak shows up as a diff.

// missionRecording is the committed shape of one golden mission: the conversational
// event stream, the final result, and the files the run left in its working
// directory (relative paths, forward-slashed, so the snapshot is identical on every
// OS).
type missionRecording struct {
	Events []mission.Event   `json:"events"`
	Result string            `json:"result"`
	Files  map[string]string `json:"files,omitempty"`
}

// recorder captures a mission's events in order.
type recorder struct {
	mu     sync.Mutex
	events []mission.Event
}

func (r *recorder) Report(_ context.Context, ev mission.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

type goldenScenario struct {
	name      string
	objective string
	turns     []llm.Response
}

func goldenScenarios() []goldenScenario {
	return []goldenScenario{
		{
			name:      "plain_answer",
			objective: "state the answer to the ultimate question",
			turns:     []llm.Response{llmtest.SayText("The answer is 42.")},
		},
		{
			name:      "write_then_done",
			objective: "create out.txt with a greeting",
			turns: []llm.Response{
				llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"out.txt","content":"hi from flynn"}`)),
				llmtest.SayText("Created out.txt."),
			},
		},
		{
			name:      "write_then_read_back",
			objective: "write greeting.txt then read it back and report",
			turns: []llm.Response{
				llmtest.CallTool("c1", "write", json.RawMessage(`{"path":"greeting.txt","content":"hello"}`)),
				llmtest.CallTool("c2", "read", json.RawMessage(`{"path":"greeting.txt"}`)),
				llmtest.SayText("The file says hello."),
			},
		},
		{
			name:      "tool_error_then_recover",
			objective: "recover from a failed tool call and finish the task",
			turns: []llm.Response{
				// An ungranted tool is refused at the waist; the model sees the error
				// result and recovers, exercising the error-result fold path with a
				// deterministic, path-free message.
				llmtest.CallTool("c1", "frobnicate", json.RawMessage(`{}`)),
				llmtest.CallTool("c2", "write", json.RawMessage(`{"path":"recovered.txt","content":"ok"}`)),
				llmtest.SayText("Recovered and wrote the file."),
			},
		},
	}
}

func TestGoldenMissions(t *testing.T) {
	for _, sc := range goldenScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			got := runGoldenMission(t, sc)
			// Replay determinism: a second identical run must produce the identical
			// recording, so the spine is a faithful fold and not order-dependent.
			again := runGoldenMission(t, sc)
			testkit.Diff(t, got, again)
			// Behaviour guard: the recording must match the committed golden.
			testkit.Golden(t, "mission_"+sc.name, got)
		})
	}
}

// runGoldenMission runs one scenario through the real assembly and returns its
// recording.
func runGoldenMission(t *testing.T, sc goldenScenario) missionRecording {
	t.Helper()
	dir := t.TempDir()
	rec := &recorder{}
	a := New(Config{WorkDir: dir})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := a.runGoal(ctx, llmtest.NewScripted(sc.turns...), sc.objective, rec)
	if err != nil {
		t.Fatalf("scenario %q: runGoal: %v", sc.name, err)
	}
	return missionRecording{Events: rec.events, Result: result, Files: materializedFiles(t, dir)}
}

// materializedFiles returns the files left under dir, keyed by forward-slashed
// relative path, so the snapshot does not depend on the OS path separator or the
// random temp directory.
func materializedFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // path is under the test's own temp dir
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk workdir: %v", err)
	}
	if len(files) == 0 {
		return nil
	}
	return files
}
