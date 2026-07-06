package main

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/externagent"
)

// TestExternalAgentSpec covers the --model spec routing: a known external-agent scheme
// selects that backend and carries its model string, a bare name selects the CLI's own
// default model, and a hosted-provider spec is left for native resolution.
func TestExternalAgentSpec(t *testing.T) {
	cases := []struct {
		spec      string
		wantName  string
		wantModel string
		wantOK    bool
	}{
		{"codex", "codex", "", true},
		{"codex:gpt-5-codex", "codex", "gpt-5-codex", true},
		{"codex:o3", "codex", "o3", true},
		{"anthropic:claude-opus-4", "", "", false},
		{"gpt-4o", "", "", false},
		{"", "", "", false},
		// A model string may itself contain a colon; only the first segment is the scheme.
		{"codex:vendor:model", "codex", "vendor:model", true},
	}
	for _, c := range cases {
		name, model, ok := externalAgentSpec(c.spec)
		if name != c.wantName || model != c.wantModel || ok != c.wantOK {
			t.Errorf("externalAgentSpec(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.spec, name, model, ok, c.wantName, c.wantModel, c.wantOK)
		}
	}
}

// TestReadinessError maps a probed Readiness to the actionable message a user sees: a
// hard refusal is terminal and named as ungovernable, an onboarding gap surfaces the
// adapter's own reason verbatim, and a ready CLI yields no error.
func TestReadinessError(t *testing.T) {
	t.Run("refusal is terminal and named", func(t *testing.T) {
		err := readinessError("codex", externagent.Readiness{
			Available: true, Version: "1.0", Refuse: true,
			Reason: "this codex build lacks the exec --json / --sandbox controls the bridge requires; update codex",
		})
		if err == nil {
			t.Fatal("a refusal must be an error")
		}
		if !strings.Contains(err.Error(), "cannot be governed") || !strings.Contains(err.Error(), "update codex") {
			t.Errorf("refusal message did not surface the reason: %v", err)
		}
	})

	t.Run("not logged in is an onboarding prompt", func(t *testing.T) {
		err := readinessError("codex", externagent.Readiness{
			Available: true, Version: "1.0",
			Reason: "codex is not logged in; run `codex login` to use your subscription",
		})
		if err == nil {
			t.Fatal("a not-ready CLI must be an error")
		}
		if !strings.Contains(err.Error(), "codex login") {
			t.Errorf("onboarding message did not name the next step: %v", err)
		}
		// The onboarding path never asks for an API key: it surfaces the CLI's reason only.
		if strings.Contains(strings.ToLower(err.Error()), "api key") {
			t.Errorf("external onboarding must not ask for an API key: %v", err)
		}
	})

	t.Run("not installed is an onboarding prompt", func(t *testing.T) {
		err := readinessError("codex", externagent.Readiness{
			Reason: "codex CLI not found on PATH; install it to use the codex backend",
		})
		if err == nil || !strings.Contains(err.Error(), "install it") {
			t.Errorf("not-installed message did not name the install step: %v", err)
		}
	})

	t.Run("ready yields no error", func(t *testing.T) {
		if err := readinessError("codex", externagent.Readiness{Available: true, LoggedIn: true, Version: "1.0"}); err != nil {
			t.Errorf("a ready CLI must not error: %v", err)
		}
	})
}

// TestExternalModel returns the CLI's model string for an external backend and empty
// for a native run, so the submitted goal pins the model only when one was selected.
func TestExternalModel(t *testing.T) {
	if got := externalModel(nil); got != "" {
		t.Errorf("externalModel(nil) = %q, want empty", got)
	}
	if got := externalModel(&externAgent{model: "gpt-5-codex"}); got != "gpt-5-codex" {
		t.Errorf("externalModel = %q, want gpt-5-codex", got)
	}
}

// TestNewExternalAgent builds a driver for a known backend and refuses an unknown one,
// so a misnamed backend fails at assembly rather than running the wrong loop.
func TestNewExternalAgent(t *testing.T) {
	ea, err := newExternalAgent("codex", t.TempDir())
	if err != nil {
		t.Fatalf("newExternalAgent(codex): %v", err)
	}
	if ea.driver == nil || ea.driver.Name() != "codex" {
		t.Errorf("driver not wired for codex: %+v", ea)
	}
	if _, err := newExternalAgent("nope", t.TempDir()); err == nil {
		t.Error("an unknown backend must be refused")
	}
}
