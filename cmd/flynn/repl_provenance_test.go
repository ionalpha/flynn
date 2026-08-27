package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestDeclareProvenanceReportsTheHarnessGap proves an externally driven run declares what
// its harness reported and says so when part of that account did not reach the record. A
// verifier reads the gap from the record; the operator only learns why it is there here.
func TestDeclareProvenanceReportsTheHarnessGap(t *testing.T) {
	s, buf := newSlashSession(t, llmtest.NewScripted())
	s.started = true
	s.runID = "external-run"
	s.ext = &externAgent{driver: &tallyingDriver{stubHarnessDriver{
		name:       "codex",
		tiers:      map[externagent.Tier]int{externagent.TierAttested: 4},
		unrecorded: 2,
		unrecErr:   errors.New("the record was closed mid-episode"),
	}}}

	s.declareProvenance(context.Background())

	if !s.provDeclared {
		t.Fatal("the run was not marked as declared, so a later seal would declare it twice")
	}
	if out := buf.String(); !strings.Contains(out, "2 attested event(s) not recorded") {
		t.Fatalf("the gap in the harness's account was not reported:\n%s", out)
	}
}

// TestDeclareProvenanceReportsAnUnwritableRecord proves a declaration that cannot be
// written is said out loud and does not end the session. It matters more than most
// reporting failures: the session continues, and a seal after this point produces a
// record that reads as natively driven, so the operator has to have been told.
func TestDeclareProvenanceReportsAnUnwritableRecord(t *testing.T) {
	s, buf := newSlashSession(t, llmtest.NewScripted())
	s.started = true
	s.runID = "external-run"
	s.ext = &externAgent{driver: &plainDriver{stubHarnessDriver{name: "claude"}}}
	// A closed store cannot take the declaration, which stands in for any stream that
	// will not accept it.
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}

	s.declareProvenance(context.Background())

	if !strings.Contains(buf.String(), "provenance not recorded") {
		t.Fatalf("a declaration that did not land was not reported:\n%s", buf.String())
	}
}

// TestDeclareProvenanceIsWrittenOnce proves a native session declares nothing (the
// absence is what says its own loop drove it), and that a run already declared is not
// declared again: a verifier reads the first declaration a record carries, so a second
// one written later would be read as the account of the whole run.
func TestDeclareProvenanceIsWrittenOnce(t *testing.T) {
	ctx := context.Background()

	native, buf := newSlashSession(t, llmtest.NewScripted())
	native.started = true
	native.runID = "native-run"
	native.declareProvenance(ctx)
	if native.provDeclared || buf.String() != "" {
		t.Fatalf("a natively driven run declared a harness:\n%s", buf.String())
	}

	s, again := newSlashSession(t, llmtest.NewScripted())
	s.started, s.runID, s.provDeclared = true, "external-run", true
	s.ext = &externAgent{driver: &plainDriver{stubHarnessDriver{name: "claude"}}}
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	// A second declaration would have to write, and the store is closed, so a write that
	// was attempted would report that it failed.
	s.declareProvenance(ctx)
	if again.String() != "" {
		t.Fatalf("an already-declared run declared again:\n%s", again.String())
	}
}
