package governed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/jobs"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// writerModel writes its goal's objective to out.txt in whatever directory it
// runs in, then answers with it. It records every objective it was asked about.
type writerModel struct {
	mu   sync.Mutex
	seen []string
}

func (m *writerModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	// The prompt appends the stop condition after the objective's own line.
	objective, _, _ := strings.Cut(req.Messages[0].Blocks[0].Text, "\n")
	m.mu.Lock()
	m.seen = append(m.seen, objective)
	m.mu.Unlock()
	last := req.Messages[len(req.Messages)-1]
	if last.Blocks[0].Kind != llm.KindToolResult {
		in, err := json.Marshal(map[string]string{"path": "out.txt", "content": objective})
		if err != nil {
			return llm.Response{}, err
		}
		return llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{
				{Kind: llm.KindToolUse, ToolUse: &llm.ToolUse{ID: "w", Name: "write", Input: in}},
			}},
			StopReason: llm.StopToolUse,
		}, nil
	}
	return llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Kind: llm.KindText, Text: "answer to " + objective}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

func (m *writerModel) objectives() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

// slowQueue claims nothing until open, standing in for a process whose worker is
// busy or polls later than its neighbour's.
type slowQueue struct {
	jobs.Queue
	open time.Time
}

func (q slowQueue) Claim(ctx context.Context, p jobs.ClaimParams) ([]jobs.Job, error) {
	if time.Now().Before(q.open) {
		return nil, nil
	}
	return q.Queue.Claim(ctx, p)
}

// processBackends opens its own handle on the shared database, as a process does.
func processBackends(t *testing.T, db string) Backends {
	t.Helper()
	st, err := sqlite.Open(context.Background(), db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg, err := Registry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return Backends{Resources: st.Resources(reg), Jobs: st.Jobs(), Log: st.Log()}
}

func assembleIn(t *testing.T, model llm.Model, b Backends, dir string) *Run {
	t.Helper()
	run, err := Assemble(context.Background(), model, b, WithWorkDir(dir))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return run
}

func startRuntime(t *testing.T, run *Run) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = run.Runtime().Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestGoalsSharingADatabaseRunOnlyInTheirOwnProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db := filepath.Join(t.TempDir(), "flynn.db")
	dirA, dirB := t.TempDir(), t.TempDir()
	modelA, modelB := &writerModel{}, &writerModel{}

	// A is up and idle before B submits, and B is slow to claim, so any step of
	// B's that A is able to take, A takes.
	runA := assembleIn(t, modelA, processBackends(t, db), dirA)
	startRuntime(t, runA)
	backB := processBackends(t, db)
	backB.Jobs = slowQueue{Queue: backB.Jobs, open: time.Now().Add(time.Second)}
	runB := assembleIn(t, modelB, backB, dirB)

	resB, err := runB.Do(ctx, goal.Spec{Objective: "goal B"})
	if err != nil {
		t.Fatalf("goal B: %v", err)
	}
	if _, err := runA.Submit(ctx, goal.Spec{Objective: "goal A"}); err != nil {
		t.Fatalf("submit A: %v", err)
	}
	textA, err := runA.Session().Wait(ctx)
	if err != nil {
		t.Fatalf("goal A: %v", err)
	}

	for _, c := range []struct {
		name, dir, text string
		model           *writerModel
	}{
		{"goal A", dirA, textA, modelA},
		{"goal B", dirB, resB.Text, modelB},
	} {
		seen := c.model.objectives()
		for _, o := range seen {
			if !strings.Contains(o, c.name) {
				t.Errorf("the process for %s ran a step of %q", c.name, o)
			}
		}
		if len(seen) == 0 {
			t.Errorf("the process for %s ran none of its own steps", c.name)
		}
		if want := "answer to " + c.name; c.text != want {
			t.Errorf("%s answered %q, want %q", c.name, c.text, want)
		}
		got, err := os.ReadFile(filepath.Join(c.dir, "out.txt"))
		if err != nil || string(got) != c.name {
			t.Errorf("%s's directory holds %q (%v), want %q", c.name, got, err, c.name)
		}
	}
}

func TestStartingARunLeavesAnotherProcessesLeaseAlone(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "flynn.db")
	other := processBackends(t, db)
	if _, err := other.Jobs.Enqueue(ctx, jobs.EnqueueParams{Queue: goal.StepQueue, Kind: goal.StepJobKind, Payload: []byte("elsewhere")}); err != nil {
		t.Fatal(err)
	}
	held, err := other.Jobs.Claim(ctx, jobs.ClaimParams{Queue: goal.StepQueue, LeaseFor: int64(time.Hour)})
	if err != nil || len(held) != 1 {
		t.Fatalf("claim: %v %v", held, err)
	}

	startRuntime(t, assembleIn(t, &writerModel{}, processBackends(t, db), t.TempDir()))
	time.Sleep(500 * time.Millisecond)

	got, err := other.Jobs.Get(ctx, held[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != jobs.StateRunning || got.LeaseExpires != held[0].LeaseExpires || got.Attempt != 1 {
		t.Fatalf("a starting process disturbed a live lease: state %s, lease %d (was %d), attempt %d",
			got.State, got.LeaseExpires, held[0].LeaseExpires, got.Attempt)
	}
}
