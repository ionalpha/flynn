package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// spineKeyPair builds a deterministic Ed25519 pair from a one-byte seed, so a test can
// state which key signed what without any randomness.
func spineTestKeys(t *testing.T, seed byte) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	s := make([]byte, ed25519.SeedSize)
	s[0] = seed
	priv := ed25519.NewKeyFromSeed(s)
	return priv, priv.Public().(ed25519.PublicKey)
}

// selfCertifyingSigner signs under the key id a verifier can recover the public key
// from, which is what lets a stored run be verified from the durable store alone.
func spineSelfSigner(t *testing.T, seed byte) (chain.RootSigner, ed25519.PublicKey) {
	t.Helper()
	priv, pub := spineTestKeys(t, seed)
	signer, err := chain.NewEd25519RootSigner(controlplane.PrincipalID(pub), priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pub
}

// namedSigner signs under an opaque key id that carries no public key, which is the
// shape of a published conformance vector: verifying it needs the key supplied.
func spineNamedSigner(t *testing.T, keyID string, seed byte) (chain.RootSigner, ed25519.PublicKey) {
	t.Helper()
	priv, pub := spineTestKeys(t, seed)
	signer, err := chain.NewEd25519RootSigner(keyID, priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, pub
}

// sealedRun writes n plain events onto a stream in a durable store under dataDir, seals
// them with signer, and closes the store, leaving exactly what a finished run leaves
// behind. It returns the run id.
func spineSealedRun(t *testing.T, dataDir string, signer chain.RootSigner, n int) string {
	t.Helper()
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const runID = "run-sealed"
	for range n {
		if _, err := store.Log().Append(ctx, spine.AppendInput{
			Stream: runID, Type: "ev", Actor: spine.ActorAgent,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sealRunFromStore(ctx, store, runID, signer); err != nil {
		t.Fatalf("seal: %v", err)
	}
	return runID
}

// TestSealRunFromStoreFoldsTheStreamAndReSeals: sealing reads the run's durable events
// (so a run continued across processes can still be sealed), refuses a run with nothing
// on it, and a re-seal folds the same events again rather than folding the earlier
// record into the chain it attests.
func TestSealRunFromStoreFoldsTheStreamAndReSeals(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	signer, pub := spineSelfSigner(t, 3)

	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if err := sealRunFromStore(ctx, store, "run/empty", signer); err == nil {
		t.Fatal("a run with no events must not be sealable")
	}

	const runID = "run/x"
	for range 4 {
		if _, err := store.Log().Append(ctx, spine.AppendInput{Stream: runID, Type: "ev", Actor: spine.ActorAgent}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sealRunFromStore(ctx, store, runID, signer); err != nil {
		t.Fatalf("seal: %v", err)
	}
	// Re-seal: the earlier record event is on the stream now, and must be skipped.
	if err := sealRunFromStore(ctx, store, runID, signer); err != nil {
		t.Fatalf("re-seal: %v", err)
	}

	events, err := store.Log().Read(ctx, spine.Query{Stream: runID})
	if err != nil {
		t.Fatal(err)
	}
	record, err := recordFromEvents(events)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	ring := chain.NewRootKeyring()
	if err := ring.Add(signer.KeyID(), pub); err != nil {
		t.Fatal(err)
	}
	folded, err := chain.VerifyRun(record, ring)
	if err != nil {
		t.Fatalf("the re-sealed record does not verify: %v", err)
	}
	if len(folded) != 4 {
		t.Fatalf("the re-seal folded %d events, want the 4 the run produced (not its own earlier record)", len(folded))
	}

	// The in-process verifier reports the same run from the open store.
	var buf bytes.Buffer
	if err := verifyStoredRun(ctx, &buf, store, runID); err != nil {
		t.Fatalf("verifyStoredRun: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "integrity:    VERIFIED") {
		t.Fatalf("verifyStoredRun report:\n%s", buf.String())
	}
	if err := verifyStoredRun(ctx, &buf, store, "run/absent"); err == nil {
		t.Fatal("verifyStoredRun accepted a run that does not exist")
	}
}

// TestRecordFromEventsRejectsAMalformedRecord: a record event that does not carry
// decodable bytes is reported, never silently treated as an unsealed run.
func TestRecordFromEventsRejectsAMalformedRecord(t *testing.T) {
	cases := map[string]map[string]any{
		"not a string": {"record": 42},
		"not base64":   {"record": "!!! not base64 !!!"},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := recordFromEvents([]spine.Event{{Type: recordEventType, Payload: payload}})
			if err == nil {
				t.Fatal("a malformed record event was accepted")
			}
			if strings.Contains(err.Error(), "was not sealed") {
				t.Fatalf("a malformed record must not read as an unsealed run: %v", err)
			}
		})
	}

	if _, err := recordFromEvents([]spine.Event{{Type: "ev"}}); err == nil {
		t.Fatal("a stream with no record event must report it was not sealed")
	}
}

// TestSpineExportWritesAVerifiableRecord: export extracts the run's signed record from
// the store and writes the same canonical bytes `spine verify --file` checks, so a third
// party can verify the run without the database.
func TestSpineExportWritesAVerifiableRecord(t *testing.T) {
	dataDir := t.TempDir()
	signer, _ := spineSelfSigner(t, 5)
	runID := spineSealedRun(t, dataDir, signer, 3)

	out := filepath.Join(t.TempDir(), "run.flynnrecord")
	if err := dispatchSpine([]string{"export", "--out", out, runID}, dataDir); err != nil {
		t.Fatalf("spine export: %v", err)
	}
	record, err := os.ReadFile(out)
	if err != nil || len(record) == 0 {
		t.Fatalf("exported record: %d bytes, %v", len(record), err)
	}
	// The file is the artifact, and the file alone verifies.
	if err := dispatchSpine([]string{"verify", "--file", out}, dataDir); err != nil {
		t.Fatalf("the exported record does not verify on its own: %v", err)
	}

	t.Run("default path", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := exportRun(dataDir, runID, ""); err != nil {
			t.Fatalf("export: %v", err)
		}
		if _, err := os.Stat(runID + ".flynnrecord"); err != nil {
			t.Fatalf("export wrote nothing at the default path: %v", err)
		}
	})
}

// TestSpineExportRefusesWhatItCannotExport: a run that was never sealed, and one that
// does not exist, are reported rather than written out as an empty or partial file.
func TestSpineExportRefusesWhatItCannotExport(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Log().Append(ctx, spine.AppendInput{
		Stream: "run/unsealed", Type: "ev", Actor: spine.ActorAgent,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "out.flynnrecord")
	err = dispatchSpine([]string{"export", "--out", dest, "run/unsealed"}, dataDir)
	if err == nil || !strings.Contains(err.Error(), "not sealed") {
		t.Fatalf("exporting an unsealed run = %v, want a refusal", err)
	}
	if _, serr := os.Stat(dest); serr == nil {
		t.Fatal("a refused export still wrote a file")
	}
	if err := dispatchSpine([]string{"export", "--out", dest, "run/absent"}, dataDir); err == nil {
		t.Fatal("exporting a run that does not exist was accepted")
	}
}

// TestSpineVerifyFileWithASuppliedKey: a record whose signer is not self-certifying (a
// published conformance vector) verifies only when its public key is supplied, and the
// refusal says so rather than reporting a broken record.
func TestSpineVerifyFileWithASuppliedKey(t *testing.T) {
	dataDir := t.TempDir()
	signer, pub := spineNamedSigner(t, "conformance-vector-1", 9)
	runID := spineSealedRun(t, dataDir, signer, 2)

	path := filepath.Join(t.TempDir(), "vector.flynnrecord")
	if err := exportRun(dataDir, runID, path); err != nil {
		t.Fatalf("export: %v", err)
	}

	// Without the key, the record names a signer nothing can recover a key from.
	err := dispatchSpine([]string{"verify", "--file", path}, dataDir)
	if err == nil || !strings.Contains(err.Error(), "--key") {
		t.Fatalf("verify without the key = %v, want a refusal naming --key", err)
	}
	// With it, the record verifies.
	if err := dispatchSpine([]string{"verify", "--file", path, "--key", hex.EncodeToString(pub)}, dataDir); err != nil {
		t.Fatalf("verify with the supplied key: %v", err)
	}

	// The wrong key of the right shape does not verify: the report is printed and the
	// command still fails.
	_, other := spineTestKeys(t, 11)
	var buf bytes.Buffer
	record, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if verr := verifyRecord(&buf, path, record, hex.EncodeToString(other)); verr == nil {
		t.Fatal("a record verified against the wrong key")
	} else if !strings.Contains(buf.String(), "integrity:    NOT VERIFIED") {
		t.Fatalf("the failing tier was not reported:\n%s", buf.String())
	}

	// A key that is not usable at all is refused before anything is checked.
	for _, bad := range []string{"zz", hex.EncodeToString([]byte("short"))} {
		if _, kerr := resolveKey("conformance-vector-1", bad); kerr == nil {
			t.Fatalf("key %q was accepted", bad)
		}
	}
}

// TestSpineVerifyFileRefusals: a file that is not there, and a file that is not a
// record, are reported as such.
func TestSpineVerifyFileRefusals(t *testing.T) {
	dir := t.TempDir()
	if err := verifyRecordFile(filepath.Join(dir, "nope.flynnrecord"), ""); err == nil {
		t.Fatal("verifying a file that does not exist was accepted")
	}
	junk := filepath.Join(dir, "junk.flynnrecord")
	if err := os.WriteFile(junk, []byte("not a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyRecordFile(junk, ""); err == nil {
		t.Fatal("verifying a file that is not a record was accepted")
	}
}

// TestSpineDispatchUsage pins the command's own argument handling: every unusable form
// is a usage error naming what the command takes, and no form silently does nothing.
func TestSpineDispatchUsage(t *testing.T) {
	dataDir := t.TempDir()
	cases := map[string][]string{
		"no subcommand":     nil,
		"unknown":           {"frobnicate"},
		"verify no run":     {"verify"},
		"verify bad flag":   {"verify", "--nope"},
		"export no run":     {"export"},
		"export bad flag":   {"export", "--nope"},
		"key without file":  {"verify", "--key", "abcd", "run/x"},
		"verify absent run": {"verify", "run/absent"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if err := dispatchSpine(args, dataDir); err == nil {
				t.Fatalf("dispatchSpine(%v) was accepted", args)
			}
		})
	}
}

// TestSpineVerifyStoredRunFromTheStore drives `flynn spine verify <run-id>` against a
// run sealed in the durable store: the signer is self-certifying, so the record verifies
// with nothing but the store.
func TestSpineVerifyStoredRunFromTheStore(t *testing.T) {
	dataDir := t.TempDir()
	signer, _ := spineSelfSigner(t, 13)
	runID := spineSealedRun(t, dataDir, signer, 5)

	if err := dispatchSpine([]string{"verify", runID}, dataDir); err != nil {
		t.Fatalf("spine verify: %v", err)
	}
}

// TestVerifyCheckpointedStreamNeedsARecoverableKey: a checkpointed stream signed under
// an opaque key id cannot be verified from the store alone, and says so rather than
// reporting a pass or a broken chain.
func TestVerifyCheckpointedStreamNeedsARecoverableKey(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	signer, _ := spineNamedSigner(t, "operator-key-1", 17)
	rec := chain.NewDurableRecorder(store.Log(),
		func(s string) chain.FlushNodeStore { return store.MerkleNodes(s) },
		store, signer, nil, 10)
	const stream = "server/opaque"
	for range 12 {
		if _, err := rec.Append(ctx, spine.AppendInput{Stream: stream, Type: "ev", Actor: spine.ActorSystem}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Checkpoint(ctx, stream); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err = verifyCheckpointedStream(&buf, store, stream)
	if err == nil {
		t.Fatal("a checkpoint under an unrecoverable key id was verified from the store alone")
	}
	if !strings.Contains(err.Error(), "--key") {
		t.Fatalf("the refusal should name what is missing, got %v", err)
	}
}

// TestSnapshotOptionsOnlyActivateUnderARecoverableKey: verified snapshots are activated
// only when the snapshot can be sealed and checked back against a key a rebuild can
// recover. With no signer, or one whose key id is opaque, the store folds from the log
// instead of trusting an unverifiable blob.
func TestSnapshotOptionsOnlyActivateUnderARecoverableKey(t *testing.T) {
	if opts := snapshotOptions(nil); opts != nil {
		t.Fatalf("no signer must activate no snapshot options, got %d", len(opts))
	}
	opaque, _ := spineNamedSigner(t, "operator-key-1", 19)
	if opts := snapshotOptions(opaque); opts != nil {
		t.Fatalf("an unrecoverable key must activate no snapshot options, got %d", len(opts))
	}
	self, _ := spineSelfSigner(t, 21)
	opts := snapshotOptions(self)
	if len(opts) != 2 {
		t.Fatalf("a self-certifying signer must activate the sealed snapshot codec and its cadence, got %d options", len(opts))
	}

	// The options are the ones a real store accepts, and a store opened under them still
	// works: a snapshot is a derived cache, never a change to what the store answers.
	ctx := context.Background()
	store, err := sqlite.Open(ctx, ":memory:", opts...)
	if err != nil {
		t.Fatalf("a store cannot be opened under the snapshot options: %v", err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.Log().Append(ctx, spine.AppendInput{Stream: "s", Type: "ev", Actor: spine.ActorSystem}); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotAndCheckpointCadence: the cadences come from the environment, a value
// that is not a usable count falls back to the default rather than disabling the
// verified checkpoint by accident, and zero disables deliberately.
func TestSnapshotAndCheckpointCadence(t *testing.T) {
	cases := []struct {
		set  string
		want int
	}{
		{"", defaultSnapshotEvery},
		{"32", 32},
		{"0", 0},
		{"-1", defaultSnapshotEvery},
		{"many", defaultSnapshotEvery},
	}
	for _, tc := range cases {
		t.Run("snapshot "+tc.set, func(t *testing.T) {
			t.Setenv("FLYNN_SNAPSHOT_EVERY", tc.set)
			if got := snapshotEvery(); got != tc.want {
				t.Fatalf("FLYNN_SNAPSHOT_EVERY=%q gave %d, want %d", tc.set, got, tc.want)
			}
		})
	}

	checks := []struct {
		set  string
		want int
	}{
		{"", defaultCheckpointEvery},
		{"64", 64},
		{"0", 0},
		{"-5", defaultCheckpointEvery},
		{"soon", defaultCheckpointEvery},
	}
	for _, tc := range checks {
		t.Run("checkpoint "+tc.set, func(t *testing.T) {
			t.Setenv("FLYNN_CHECKPOINT_EVERY", tc.set)
			if got := checkpointEvery(); got != tc.want {
				t.Fatalf("FLYNN_CHECKPOINT_EVERY=%q gave %d, want %d", tc.set, got, tc.want)
			}
		})
	}
}

// TestRunSignerIsStableAndSelfCertifying: the instance's signing identity is created on
// first use and reused afterwards, and its key id carries the public key, which is what
// lets a sealed run be verified with nothing but the record.
func TestRunSignerIsStableAndSelfCertifying(t *testing.T) {
	ctx := context.Background()
	dataDir := fileVaultEnv(t)

	first, err := runSigner(ctx, dataDir)
	if err != nil {
		t.Fatalf("runSigner: %v", err)
	}
	again, err := runSigner(ctx, dataDir)
	if err != nil {
		t.Fatalf("runSigner (second call): %v", err)
	}
	if first.KeyID() != again.KeyID() {
		t.Fatalf("the instance identity is not stable: %q then %q", first.KeyID(), again.KeyID())
	}
	if _, err := controlplane.ParsePrincipalID(first.KeyID()); err != nil {
		t.Fatalf("the run signer's key id is not self-certifying: %v", err)
	}

	// A run sealed under it verifies from the durable store with no key supplied.
	runID := spineSealedRun(t, dataDir, first, 2)
	if err := verifyRun(dataDir, runID); err != nil {
		t.Fatalf("a run sealed under the instance identity does not verify: %v", err)
	}
}

// TestExportRecordRefusesAnUndecodableRecord: a record event whose payload cannot be
// decoded is reported against the run, and nothing is written.
func TestExportRecordRefusesAnUndecodableRecord(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const runID = "run/bad"
	if _, err := store.Log().Append(ctx, spine.AppendInput{
		Stream: runID, Type: recordEventType, Actor: spine.ActorSystem,
		Payload: map[string]any{"record": "not-base64!!"},
	}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out.flynnrecord")
	err = exportRecord(ctx, store, runID, dest)
	if err == nil || !strings.Contains(err.Error(), runID) {
		t.Fatalf("exportRecord = %v, want a refusal naming the run", err)
	}
	if _, serr := os.Stat(dest); serr == nil {
		t.Fatal("a refused export wrote a file anyway")
	}
}

// TestAttestedKindsIsStable: the harness's own account is summarised in a fixed order,
// so two verifications of one record read identically.
func TestAttestedKindsIsStable(t *testing.T) {
	events := []chain.AttestedEvent{
		{Kind: "text"}, {Kind: "bridge_call"}, {Kind: "text"}, {Kind: "text"}, {Kind: "bridge_call"},
	}
	got := attestedKinds(events)
	if got != "bridge_call x2, text x3" {
		t.Fatalf("attestedKinds = %q", got)
	}
	if attestedKinds(nil) != "" {
		t.Fatalf("no attested events must summarise to nothing, got %q", attestedKinds(nil))
	}
}
