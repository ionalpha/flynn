package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/inbox"
	"github.com/ionalpha/flynn/notices"
	"github.com/ionalpha/flynn/spine"
)

// These tests drive the entry points that take the operator's shell policy from the command
// line (a goal, a resumed run, the served channels) against a model on loopback, and read
// the posture each one recorded. The assembly tests elsewhere call the assemblers directly,
// so they cannot notice an entry point that stops passing the policy on.

// relaxedShell differs from the zero policy on both dials, so a posture that reads it back
// proves the entry point passed the operator's policy and not the strictest default.
var relaxedShell = shellPolicy{allowNetwork: true, allowCredentialReads: true}

// shellRunEnv prepares a process that can resolve the model "openai:gpt-test" with no
// operator at a terminal, in a fresh working directory and home, and returns the data
// directory. The model is a server on loopback that refuses every request, so a run ends at
// its first model call: the posture is recorded when the run is assembled, before that call,
// and these tests read nothing the model would have said. A run takes its provider key out of
// the process environment, so the returned function sets it again for a second run.
func shellRunEnv(t *testing.T) (dataDir string, provide func()) {
	t.Helper()
	dataDir = fileVaultEnv(t)
	t.Setenv(notices.OffEnv, "1")
	useHome(t)
	t.Chdir(t.TempDir())
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"refused by the test"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(model.Close)
	provide = func() {
		t.Setenv("OPENAI_API_KEY", "test-key")
		t.Setenv("OPENAI_BASE_URL", model.URL+"/v1")
	}
	provide()
	prior := modelSpecExplicit
	modelSpecExplicit = true
	t.Cleanup(func() { modelSpecExplicit = prior })
	return dataDir, provide
}

// runIDPattern picks the run id out of the line a run prints before it starts.
var runIDPattern = regexp.MustCompile(`(?m)^\s+run (\S+)\s*$`)

// storedPostures returns every shell posture recorded on stream in the data directory.
func storedPostures(t *testing.T, dataDir, stream string) []map[string]any {
	t.Helper()
	store, err := openDataStore(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	return posturesOn(t, store.Log(), stream)
}

// TestGoalAndResumeRecordTheOperatorsShellPolicy: a goal run records the policy it was
// started with, and a resumed run records the policy of the resume, which is the one its
// commands ran under. A resume that fell back to the default would record the stricter
// posture here while the operator believed they had relaxed it.
func TestGoalAndResumeRecordTheOperatorsShellPolicy(t *testing.T) {
	dataDir, provide := shellRunEnv(t)
	stdout := captureStdout(t)

	// The refusing model ends the run with an error; the run was assembled and recorded first.
	_ = runGoal("openai:gpt-test", "say hello", "", dataDir, goalSpecFile{}, false, false, false, false, nil, nil, nil, 0, 0, relaxedShell)
	m := runIDPattern.FindStringSubmatch(stdout())
	if m == nil {
		t.Fatalf("the goal printed no run id: %q", stdout())
	}
	runID := m[1]

	got := storedPostures(t, dataDir, runID)
	if len(got) != 1 || got[0]["network"] != "open" || got[0]["credentials"] != "readable" {
		t.Fatalf("goal postures = %v, want one with the network open and credentials readable", got)
	}

	// The resume relaxes only the credentials, so a posture that repeated the goal's would
	// show the policy was not the resume's.
	provide()
	_ = resumeRun("openai:gpt-test", runID, dataDir, false, checkShell)
	got = storedPostures(t, dataDir, runID)
	if len(got) != 2 || got[1]["network"] != "denied" || got[1]["credentials"] != "readable" {
		t.Fatalf("postures after the resume = %v, want a second one with the network denied and credentials readable", got)
	}
}

// appendedLog is a log that remembers what was appended to it, so a test can read the
// streams a run wrote without knowing the id it was given.
type appendedLog struct {
	spine.Log
	mu  sync.Mutex
	got []spine.AppendInput
}

func (l *appendedLog) Append(ctx context.Context, in spine.AppendInput) (spine.Event, error) {
	l.mu.Lock()
	l.got = append(l.got, in)
	l.mu.Unlock()
	return l.Log.Append(ctx, in)
}

// postures returns the payload of each shell posture event appended so far.
func (l *appendedLog) postures() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, in := range l.got {
		if in.Type == shellPostureEvent {
			out = append(out, in.Payload)
		}
	}
	return out
}

// silentSource is a channel that never delivers a message and ends when the run does, so the
// served path has the one source it requires and nothing to answer.
type silentSource struct{}

func (silentSource) Name() string { return "silent" }

func (silentSource) Receive(ctx context.Context) (<-chan inbox.Spec, error) {
	ch := make(chan inbox.Spec)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// TestServedChannelsRecordTheOperatorsShellPolicy: the message-answering path assembles its
// mission under the policy it was handed, which is what makes the serve flags reach a
// command a channel message triggers.
func TestServedChannelsRecordTheOperatorsShellPolicy(t *testing.T) {
	dataDir, _ := shellRunEnv(t)
	store := memStore(t)
	log := &appendedLog{Log: store.Log()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := serveChannels(ctx, "openai:gpt-test", dataDir, store.Resources(mustRegistry(t)), store.Jobs(), log, []inbox.Source{silentSource{}}, nil, relaxedShell)
	if err != nil {
		t.Fatalf("serve channels: %v", err)
	}

	got := log.postures()
	if len(got) != 1 || got[0]["network"] != "open" || got[0]["credentials"] != "readable" {
		t.Fatalf("served postures = %v, want one with the network open and credentials readable", got)
	}
}
