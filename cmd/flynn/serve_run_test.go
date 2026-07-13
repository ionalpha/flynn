package main

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/notices"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/resource"
)

// serveEnv prepares a server-under-test environment: a fresh data directory, the notice
// channel off, and no provider credential inherited from the developer's environment (a
// resolved model would make the server go on to do real work). The returned spec names a
// provider explicitly, so a missing credential is refused rather than answered by
// whichever other provider happens to be configured.
func serveEnv(t *testing.T) (dataDir, modelSpec string) {
	t.Helper()
	t.Setenv(notices.OffEnv, "1")
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("FLYNN_API_TOKEN", "")
	for _, k := range provider.CredentialEnvVars() {
		t.Setenv(k, "")
	}
	prior := modelSpecExplicit
	modelSpecExplicit = true
	t.Cleanup(func() { modelSpecExplicit = prior })
	return t.TempDir(), "anthropic:claude-opus-4-8"
}

// TestServeRejectsBadFlags: the serve flag set hands parse errors back rather than
// exiting, so a typo is an error the dispatch turns into an exit code.
func TestServeRejectsBadFlags(t *testing.T) {
	dataDir, spec := serveEnv(t)
	if err := runServeContext(context.Background(), []string{"--no-such-flag"}, spec, dataDir); err == nil {
		t.Fatal("expected an unknown flag to be refused")
	}
}

// TestServeRefusesAnExternalAgent: an external agent CLI drives its own loop, so it
// cannot back the served path. It is refused before anything is opened.
func TestServeRefusesAnExternalAgent(t *testing.T) {
	dataDir, _ := serveEnv(t)
	err := runServeContext(context.Background(), nil, "codex:gpt-5", dataDir)
	if err == nil {
		t.Fatal("expected serve to refuse an external agent backend")
	}
	if !strings.Contains(err.Error(), "codex") {
		t.Fatalf("error = %v, want it to name the backend it refused", err)
	}
}

// TestServeWithNothingConfigured: a serve with no channel and no API has nothing to do,
// and says so rather than idling forever waiting for an interrupt.
func TestServeWithNothingConfigured(t *testing.T) {
	dataDir, spec := serveEnv(t)
	err := runServeContext(context.Background(), nil, spec, dataDir)
	if err == nil {
		t.Fatal("expected serve with no channel and no API to be refused")
	}
	if !strings.Contains(err.Error(), "nothing to do") {
		t.Fatalf("error = %v, want the nothing-to-do refusal", err)
	}
}

// TestServeRefusesAnUnsafeAPIBind: the API listener opens through the inbound gate, which
// refuses a wildcard bind outright. The bind is checked before the socket opens, so an
// unsafe address fails closed and nothing is ever exposed. Both authenticator paths (an
// operator-supplied token, and the one generated when none is given) run into the same
// gate, so neither can serve openly on a wildcard.
func TestServeRefusesAnUnsafeAPIBind(t *testing.T) {
	cases := map[string][]string{
		"generated token": {"--api-addr", "0.0.0.0:0"},
		"supplied token":  {"--api-addr", "0.0.0.0:0", "--api-token", "operator-token"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir, spec := serveEnv(t)
			err := runServeContext(context.Background(), args, spec, dataDir)
			if err == nil {
				t.Fatal("a wildcard bind must be refused")
			}
			if !strings.Contains(err.Error(), "serve: api:") {
				t.Fatalf("error = %v, want the API bind to be what failed", err)
			}
		})
	}
}

// TestServeMonitorOnlyServesAndStops is the monitor-only daemon end to end: it binds the
// read-only API on loopback, answers on it (unauthenticated requests are refused, which
// is the secured-by-default property), and shuts down cleanly when the context is
// cancelled, exactly as Ctrl-C cancels it in the real command.
func TestServeMonitorOnlyServesAndStops(t *testing.T) {
	dataDir, spec := serveEnv(t)
	addr := freeLoopbackAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- runServeContext(ctx, []string{"--api-addr", addr, "--api-token", "operator-token"}, spec, dataDir)
	}()

	waitForListener(t, addr, errc)
	cancel()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("a cancelled serve must stop cleanly, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop after its context was cancelled")
	}
}

// TestServeChannelNeedsAModel: with a channel configured the server needs a model to
// answer with, so a spec whose provider has no credential fails at start rather than
// accepting messages it could never answer. Both channel adapters are assembled on the
// way there, which is what makes this the configured-channel path.
func TestServeChannelNeedsAModel(t *testing.T) {
	dataDir, spec := serveEnv(t)
	args := []string{"--telegram-token", "test-token", "--signal-tcp", "127.0.0.1:7583"}
	err := runServeContext(context.Background(), args, spec, dataDir)
	if err == nil {
		t.Fatal("expected serve to refuse to start with no model credential")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Fatalf("error = %v, want the missing credential to be the reason", err)
	}
}

// TestServeRefusesAnEmptyChannelToken: an empty --signal-tcp is not a channel, and the
// adapter says so rather than being assembled into a source that can never connect.
func TestServeRefusesAnEmptyChannelToken(t *testing.T) {
	dataDir, spec := serveEnv(t)
	// An empty value for a flag that was given is not the same as the flag being absent:
	// the absent case is covered by the nothing-to-do refusal above.
	t.Setenv("TELEGRAM_BOT_TOKEN", "from-the-environment")
	err := runServeContext(context.Background(), nil, spec, dataDir)
	if err == nil {
		t.Fatal("expected serve to get past the nothing-to-do refusal with a channel from the environment")
	}
	if strings.Contains(err.Error(), "nothing to do") {
		t.Fatalf("a token from the environment must configure the channel, got %v", err)
	}
}

// freeLoopbackAddr returns a loopback address that was bindable a moment ago, so the
// server under test can be given a real port without hard-coding one.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

// waitForListener blocks until the server under test accepts a connection on addr, or
// fails first. It reports the server's own error rather than a timeout when the server is
// what went wrong.
func waitForListener(t *testing.T, addr string, errc <-chan error) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case serr := <-errc:
			t.Fatalf("serve exited before the API was listening: %v", serr)
		case <-deadline.C:
			t.Fatalf("the API never came up on %s", addr)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// goalGetStore serves one goal resource by name, so the worker's terminal-phase mapping
// is exercised without a runtime behind it.
type goalGetStore struct {
	resource.Store
	res resource.Resource
	err error
}

func (g goalGetStore) Get(context.Context, string, resource.Scope, string) (resource.Resource, error) {
	return g.res, g.err
}

// TestGoalWorkerPoll: triage asks the worker whether the goal it submitted is finished. A
// converged goal is done and succeeded; a stalled one is done and failed; anything else is
// still running, and a store that cannot answer is an error rather than a guess.
func TestGoalWorkerPoll(t *testing.T) {
	cases := []struct {
		name       string
		phase      goal.Phase
		wantDone   bool
		wantFailed bool
	}{
		{"converged", goal.PhaseConverged, true, false},
		{"stalled", goal.PhaseStalled, true, true},
		{"running", goal.PhaseRunning, false, false},
		{"pending", goal.PhasePending, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, err := json.Marshal(goal.Status{Phase: tc.phase, Message: "the answer"})
			if err != nil {
				t.Fatalf("marshal status: %v", err)
			}
			w := &goalWorker{store: goalGetStore{res: resource.Resource{Kind: goal.Kind, Name: "g-1", Status: status}}}
			done, answer, failed, err := w.Poll(context.Background(), "g-1")
			if err != nil {
				t.Fatalf("poll: %v", err)
			}
			if done != tc.wantDone || failed != tc.wantFailed {
				t.Fatalf("done=%v failed=%v, want done=%v failed=%v", done, failed, tc.wantDone, tc.wantFailed)
			}
			if tc.wantDone && answer != "the answer" {
				t.Fatalf("answer = %q, want the goal's final message", answer)
			}
			if !tc.wantDone && answer != "" {
				t.Fatalf("an unfinished goal must not answer, got %q", answer)
			}
		})
	}
}

// TestGoalWorkerPollErrors: a goal that cannot be read, or whose status cannot be
// decoded, is an error. Reporting it as unfinished would park triage on it forever.
func TestGoalWorkerPollErrors(t *testing.T) {
	t.Run("unreadable goal", func(t *testing.T) {
		w := &goalWorker{store: goalGetStore{err: resource.ErrNotFound}}
		if _, _, _, err := w.Poll(context.Background(), "gone"); err == nil {
			t.Fatal("expected a store error to be reported")
		}
	})
	t.Run("undecodable status", func(t *testing.T) {
		res := resource.Resource{Kind: goal.Kind, Name: "g-1", Status: json.RawMessage(`"not an object"`)}
		w := &goalWorker{store: goalGetStore{res: res}}
		if _, _, _, err := w.Poll(context.Background(), "g-1"); err == nil {
			t.Fatal("expected an undecodable status to be reported")
		}
	})
}
