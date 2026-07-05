package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/llm/llmtest"
)

// TestExportRecordWritesVerifiableFile drives a run to convergence, seals it, exports the
// sealed record to a file, and verifies that file on its own. It proves /export (and
// `flynn spine export`) produce the same portable artifact `flynn spine verify --file`
// checks: the written bytes rebuild the signed root and pass governance from the file
// alone, with no durable store.
func TestExportRecordWritesVerifiableFile(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}

	model := llmtest.NewScripted(llmtest.SayText("done"))
	_, runID, _, err := drive(ctx, io.Discard, model, harness.Plan{}, t.TempDir(),
		"reply done and stop", defaultSystemPrompt,
		store.Resources(reg), store.Jobs(), store.Log(), false, "", nil)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}

	if err := sealRunFromStore(ctx, store, runID, selfCertifyingSigner(t)); err != nil {
		t.Fatalf("seal: %v", err)
	}

	path := filepath.Join(t.TempDir(), runID+".flynnrecord")
	if err := exportRecord(ctx, store, runID, path); err != nil {
		t.Fatalf("export: %v", err)
	}

	// The file is non-empty and re-verifies from itself, the way a third party would check
	// it: the record's signer is self-certifying, so no --key is needed.
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("exported record missing or empty: %v", err)
	}
	record, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := verifyRecord(&buf, path, record, ""); err != nil {
		t.Fatalf("verify exported record: %v\n%s", err, buf.String())
	}
	report := buf.String()
	for _, want := range []string{"integrity:", "VERIFIED", "governance:", "OK"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

// TestExportUnsealedRunRefused proves exporting a run that was never sealed is refused
// rather than writing a partial or empty file: the stream carries no signed record, so
// there is nothing portable to hand out.
func TestExportUnsealedRunRefused(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		t.Fatal(err)
	}

	model := llmtest.NewScripted(llmtest.SayText("done"))
	_, runID, _, err := drive(ctx, io.Discard, model, harness.Plan{}, t.TempDir(),
		"reply done and stop", defaultSystemPrompt,
		store.Resources(reg), store.Jobs(), store.Log(), false, "", nil)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}

	path := filepath.Join(t.TempDir(), runID+".flynnrecord")
	if err := exportRecord(ctx, store, runID, path); err == nil {
		t.Fatal("exporting an unsealed run should error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no file should be written for an unsealed run, stat err = %v", err)
	}
}

// TestExportMissingRunRefused proves exporting an unknown run id is refused rather than
// writing a file, so a typo never yields an empty record.
func TestExportMissingRunRefused(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	path := filepath.Join(t.TempDir(), "nonesuch.flynnrecord")
	if err := exportRecord(ctx, store, "nonesuch", path); err == nil {
		t.Fatal("exporting an unknown run should error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no file should be written for an unknown run, stat err = %v", err)
	}
}
