package e2e

import (
	"os"
	"testing"
)

// TestSpineTamperDetected proves the headline claim end to end: a clean run's record
// verifies, and any single-byte mutation of the exported record is rejected. It exports
// the portable record, flips one byte, and asserts `spine verify --file` fails with a
// root-mismatch (integrity NOT VERIFIED) and a non-zero exit, so a script gating on the
// record cannot be fooled by a tampered artifact.
func TestSpineTamperDetected(t *testing.T) {
	fake := newFakeOpenAI(t, finalText("the result"))
	in := newInstance(t).withModel(fake)

	res := in.run("-no-learn", "goal", "produce a record")
	requireExit(t, res, 0, "goal")
	runID := in.runID(res)

	// Clean record verifies from the file path too.
	path, exp := in.export(runID)
	requireExit(t, exp, 0, "spine export")
	clean := in.run("spine", "verify", "--file", path)
	requireExit(t, clean, 0, "verify clean record file")
	requireContains(t, clean.stdout, "VERIFIED", "clean verify")

	// Flip one byte in the middle of the record and re-verify: it must fail.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 8 {
		t.Fatalf("record suspiciously small: %d bytes", len(raw))
	}
	raw[len(raw)/2] ^= 0x01
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	tampered := in.run("spine", "verify", "--file", path)
	if tampered.code == 0 {
		t.Fatalf("tampered record verified as clean; exit 0\nstdout:\n%s", tampered.stdout)
	}
	requireContains(t, tampered.combined(), "NOT VERIFIED", "tampered verify report")
}
