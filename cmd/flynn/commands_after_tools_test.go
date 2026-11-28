package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// toolTurnModel calls the glob tool on the first request that offers it, answers in
// text after that, and records every request [llmtest.CheckRequest] rejects. A command
// that sends a provider history it would refuse is then a test failure even when the
// command reports the error to the user and carries on.
type toolTurnModel struct {
	mu       sync.Mutex
	called   bool
	rejected []string
}

func (m *toolTurnModel) Generate(_ context.Context, req llm.Request) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := llmtest.CheckRequest(req); err != nil {
		m.rejected = append(m.rejected, err.Error())
		return llm.Response{}, err
	}
	if !m.called {
		for _, tool := range req.Tools {
			if tool.Name == "glob" {
				m.called = true
				return llmtest.CallTool("t1", "glob", []byte(`{"pattern":"*"}`)), nil
			}
		}
	}
	return llmtest.SayText("the user asked to look around; glob ran"), nil
}

func (m *toolTurnModel) check(t *testing.T, cmd string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.called {
		t.Fatal("the opening turn never called a tool, so the commands ran on a history without one")
	}
	for _, r := range m.rejected {
		t.Errorf("%s sent a request a provider would reject: %s", cmd, r)
	}
}

// TestEveryCommandAfterAToolCall runs each command, in both interfaces, on a session
// whose last turn called a tool. A session that never called one is the easy case: its
// history is plain text, which every provider takes. /compact passed its tests and failed
// every real session with HTTP 400, because its tests ran on turns without a tool call.
func TestEveryCommandAfterAToolCall(t *testing.T) {
	for _, c := range sessionCommands() {
		t.Run("line"+c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			m := &toolTurnModel{}
			s, buf := newSlashSession(t, m)
			if _, err := s.runTurn(ctx, "look around", nil, nil); err != nil {
				t.Fatalf("turn: %v\n%s", err, buf.String())
			}
			handled, err := s.replCommand(ctx, c.name)
			if !handled {
				t.Fatalf("%s was not run as a command", c.name)
			}
			if err != nil && strings.Contains(err.Error(), "would reject") {
				t.Errorf("%s: %v", c.name, err)
			}
			m.check(t, c.name)
		})
		t.Run("tui"+c.name, func(t *testing.T) {
			m := &toolTurnModel{}
			host, ui := newHostForTest(t, m)
			host.submit("look around", nil)
			waitIdle(t, host)
			host.submit(c.name, nil)
			waitIdle(t, host)
			if strings.Contains(ui.transcript(), "would reject") {
				t.Errorf("%s reported a rejected request:\n%s", c.name, ui.transcript())
			}
			m.check(t, c.name)
		})
	}
}

// TestCompactAfterAToolCallSucceeds is the case the HTTP 400 came from, held to the
// outcome and not only to the request: the compaction completes and carries a summary.
func TestCompactAfterAToolCallSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m := &toolTurnModel{}
	s, buf := newSlashSession(t, m)
	if _, err := s.runTurn(ctx, "look around", nil, nil); err != nil {
		t.Fatalf("turn: %v\n%s", err, buf.String())
	}
	if handled, err := s.replCommand(ctx, "/compact"); !handled || err != nil {
		t.Fatalf("/compact = (%v, %v)\n%s", handled, err, buf.String())
	}
	m.check(t, "/compact")
	if !strings.Contains(s.carriedContext, "glob ran") {
		t.Fatalf("carried context = %q", s.carriedContext)
	}
}
