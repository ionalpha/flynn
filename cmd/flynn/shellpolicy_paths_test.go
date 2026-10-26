package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/governed"
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
var relaxedShell = governed.ShellPolicy{AllowNetwork: true, AllowCredentialReads: true}

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

// posturesOn returns the payload of every shell posture event on stream, oldest first. A
// resumed run assembles again and states its posture again, so its stream holds one per
// assembly.
func posturesOn(t *testing.T, log spine.Log, stream string) []map[string]any {
	t.Helper()
	events, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	var found []map[string]any
	for _, e := range events {
		if e.Type == governed.ShellPostureEventType {
			found = append(found, e.Payload)
		}
	}
	return found
}

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
		if in.Type == governed.ShellPostureEventType {
			out = append(out, in.Payload)
		}
	}
	return out
}

// silentSource is a channel that never delivers a message, so the served path has the one
// source it requires and nothing to answer. The served loop reads its sources only after the
// run is assembled, so assembled is called once the posture is on the record; a test passes
// the cancel of the context the loop runs under to end the loop there. A context cancelled
// earlier would fail the assembly's own record, which is not what the test is about.
type silentSource struct{ assembled func() }

func (silentSource) Name() string { return "silent" }

func (s silentSource) Receive(ctx context.Context) (<-chan inbox.Spec, error) {
	if s.assembled != nil {
		s.assembled()
	}
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &appendedLog{Log: store.Log()}

	err := serveChannels(ctx, "openai:gpt-test", dataDir, store.Resources(mustRegistry(t)), store.Jobs(), log, []inbox.Source{silentSource{assembled: cancel}}, nil, senderAllowList{}, serveGovernance{shell: relaxedShell})
	if err != nil {
		t.Fatalf("serve channels: %v", err)
	}

	got := log.postures()
	if len(got) != 1 || got[0]["network"] != "open" || got[0]["credentials"] != "readable" {
		t.Fatalf("served postures = %v, want one with the network open and credentials readable", got)
	}
}

// mcpSessionPattern picks the session id out of the line the MCP server logs when it starts.
var mcpSessionPattern = regexp.MustCompile(`session (\S+)
`)

// TestMCPServerRecordsTheDefaultShellPosture: the tool host a client drives builds its
// sandbox under the strictest policy and names the data directory it hides, so a client's
// shell call cannot read the instance's signing key or reach the network. The server takes
// no shell flags, so the default is the only posture it can have.
func TestMCPServerRecordsTheDefaultShellPosture(t *testing.T) {
	mcpTestEnv(t)
	useHome(t)
	dataDir := t.TempDir()

	var out, logw bytes.Buffer
	if err := serveMCP(context.Background(), dataDir, t.TempDir(), false, strings.NewReader(""), &out, &logw); err != nil {
		t.Fatalf("serveMCP: %v", err)
	}
	m := mcpSessionPattern.FindStringSubmatch(logw.String())
	if m == nil {
		t.Fatalf("the server logged no session id: %q", logw.String())
	}

	got := storedPostures(t, dataDir, m[1])
	if len(got) != 1 || got[0]["network"] != "denied" || got[0]["credentials"] != "hidden" {
		t.Fatalf("MCP postures = %v, want one with the network denied and credentials hidden", got)
	}
	// The sandbox records a path it can resolve by its real location, so a data directory under a
	// symlinked temp root (macOS) is named by the target.
	want, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(toStrings(got[0]["hidden"]), want) {
		t.Errorf("the MCP server's shell does not hide the data directory %q: %v", want, got[0]["hidden"])
	}
}

// toStrings reads a recorded list back whichever form the log holds it in.
func toStrings(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, len(l))
		for i, e := range l {
			out[i] = fmt.Sprint(e)
		}
		return out
	}
	return nil
}
