package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/inference/modelsource"
	"github.com/ionalpha/flynn/sandbox"
)

func surface(trust sandbox.Trust) modelsource.RiskSurface {
	return modelsource.RiskSurface{
		Source:      "hf:rando/x",
		Trust:       trust,
		TrustReason: "test",
		Required:    sandbox.Required(trust),
		Integrity:   modelsource.IntegrityUnverified,
		Egress:      "no network",
	}
}

func TestConsentTrustedNeedsNoPrompt(t *testing.T) {
	var out bytes.Buffer
	// A trusted catalog model is not risky: no prompt, allowed, even non-interactively.
	if err := requireConsent(surface(sandbox.TrustTrusted), false, false, strings.NewReader(""), &out); err != nil {
		t.Fatalf("trusted source must not require consent, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a trusted run must not prompt, wrote %q", out.String())
	}
}

func TestConsentNonInteractiveRefuses(t *testing.T) {
	var out bytes.Buffer
	// A risky source in a non-interactive session must be refused, never assumed yes.
	err := requireConsent(surface(sandbox.TrustUntrusted), false, false, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "non-interactive") {
		t.Fatalf("non-interactive risky run must be refused, got %v", err)
	}
}

func TestConsentAutoApproveProceedsAndLogs(t *testing.T) {
	var out bytes.Buffer
	// An explicit pre-approval proceeds and records the deliberate choice.
	if err := requireConsent(surface(sandbox.TrustUntrusted), false, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("explicit --yes must proceed, got %v", err)
	}
	if !strings.Contains(out.String(), "explicit consent") {
		t.Fatalf("an auto-approved risky run must be logged, wrote %q", out.String())
	}
}

func TestConsentInteractiveDefaultsToNo(t *testing.T) {
	var out bytes.Buffer
	// Just pressing enter (empty answer) is the safe default: declined.
	err := requireConsent(surface(sandbox.TrustUntrusted), true, false, strings.NewReader("\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("the default answer must decline, got %v", err)
	}
}

func TestConsentInteractiveYesProceeds(t *testing.T) {
	var out bytes.Buffer
	if err := requireConsent(surface(sandbox.TrustSemi), true, false, strings.NewReader("y\n"), &out); err != nil {
		t.Fatalf("an explicit yes must proceed, got %v", err)
	}
}

func TestTakeFlag(t *testing.T) {
	found, rest := takeFlag([]string{"id", "--yes", "the prompt"}, "--yes", "-y")
	if !found || len(rest) != 2 || rest[0] != "id" || rest[1] != "the prompt" {
		t.Fatalf("takeFlag mishandled the flag: found=%v rest=%v", found, rest)
	}
	found, rest = takeFlag([]string{"id", "prompt"}, "--yes", "-y")
	if found || len(rest) != 2 {
		t.Fatalf("takeFlag should not find an absent flag: found=%v rest=%v", found, rest)
	}
}
