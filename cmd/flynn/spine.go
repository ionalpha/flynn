package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// recordEventType is the event a sealed run's signed record is stored under, on the
// run's own stream, so the run can be verified from the durable store alone.
const recordEventType = "spine.record"

// runSigner loads, or on first use creates, the instance's stable identity from the
// vault under dataDir and returns a signer over it. The key id is the identity's
// self-certifying principal id, so a verifier can recover the public key from a
// signed record with no shared state.
func runSigner(ctx context.Context, dataDir string) (chain.RootSigner, error) {
	v := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase))
	id, err := controlplane.LoadOrCreateIdentity(ctx, v, "")
	if err != nil {
		return nil, err
	}
	return chain.NewEd25519RootSigner(id.ID(), ed25519.NewKeyFromSeed(id.Seed()))
}

// defaultSnapshotEvery is the automatic snapshot cadence: how many resource
// mutations pass between verified checkpoints. Overridable with
// FLYNN_SNAPSHOT_EVERY (0 disables automatic snapshots). Sealing a snapshot folds
// the stream prefix once, so the cadence trades that write-side cost against how
// many events a rebuild must fold past the last checkpoint.
const defaultSnapshotEvery = 256

// snapshotOptions builds the store options that activate verified snapshots under
// the instance signer: snapshots are sealed (checkpoint-bound, COSE-signed) with
// the run key and verified against its self-certifying public key on rebuild.
// With no signer it returns nothing: an unverified snapshot would be a trust hole
// ("just believe this blob"), so without a key the store folds from the log alone.
func snapshotOptions(signer chain.RootSigner) []sqlite.Option {
	if signer == nil {
		return nil
	}
	ss, ok := signer.(chain.SnapshotSigner)
	if !ok {
		return nil
	}
	pub, err := controlplane.ParsePrincipalID(signer.KeyID())
	if err != nil {
		return nil
	}
	ring := chain.NewRootKeyring()
	if err := ring.Add(signer.KeyID(), pub); err != nil {
		return nil
	}
	sealer, err := chain.NewSnapshotSealer(ss, ring, nil)
	if err != nil {
		return nil
	}
	return []sqlite.Option{sqlite.WithSnapshotCodec(sealer), sqlite.WithSnapshotEvery(snapshotEvery())}
}

// snapshotEvery resolves the automatic snapshot cadence from the environment,
// defaulting to defaultSnapshotEvery. Zero disables automatic snapshots.
func snapshotEvery() int {
	v := os.Getenv("FLYNN_SNAPSHOT_EVERY")
	if v == "" {
		return defaultSnapshotEvery
	}
	k, err := strconv.Atoi(v)
	if err != nil || k < 0 {
		return defaultSnapshotEvery
	}
	return k
}

// defaultCheckpointEvery is how many recorded events pass between automatic signed
// checkpoints of a served stream's durable Merkle log. A checkpoint flushes the tiles
// and signs the head, so the cadence trades write-side work against how many events a
// restart must re-fold past the last checkpoint. Overridable with FLYNN_CHECKPOINT_EVERY.
const defaultCheckpointEvery = 256

// checkpointEvery resolves the automatic checkpoint cadence from the environment,
// defaulting to defaultCheckpointEvery. Zero checkpoints only on an explicit call.
func checkpointEvery() int {
	v := os.Getenv("FLYNN_CHECKPOINT_EVERY")
	if v == "" {
		return defaultCheckpointEvery
	}
	k, err := strconv.Atoi(v)
	if err != nil || k < 0 {
		return defaultCheckpointEvery
	}
	return k
}

// sealRun seals the recorded stream and stores the signed record as one event on the
// run's stream, so the run is verifiable from the durable store afterwards. The
// record event is appended through the underlying log, not the recorder, so it is
// not itself folded into the chain it attests.
func sealRun(ctx context.Context, store *sqlite.Store, rec *chain.RecordingLog, runID string, signer chain.RootSigner) error {
	sealed, err := rec.Seal(runID, signer)
	if err != nil {
		return err
	}
	record, err := sealed.Marshal()
	if err != nil {
		return err
	}
	_, err = store.Log().Append(ctx, spine.AppendInput{
		Stream:  runID,
		Type:    recordEventType,
		Actor:   spine.ActorSystem,
		Payload: map[string]any{"record": base64.StdEncoding.EncodeToString(record)},
	})
	return err
}

// errChecksFailed reports that a record did not pass every check. The per-tier detail
// is written to stdout; this is the concise error that sets a non-zero exit code so
// the command can gate a script.
var errChecksFailed = errors.New("record did not pass all checks")

// dispatchSpine handles the spine subcommands.
func dispatchSpine(args []string, dataDir string) error {
	const usage = "usage: flynn spine verify [--file <path> [--key <hex>]] <run-id>"
	if len(args) >= 1 && args[0] == "verify" {
		fs := flag.NewFlagSet("spine verify", flag.ContinueOnError)
		file := fs.String("file", "", "verify a record read from this file instead of a stored run")
		keyHex := fs.String("key", "", "hex-encoded Ed25519 public key to verify a record whose signer is not self-certifying (for example a published conformance vector)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *file != "" {
			return verifyRecordFile(*file, *keyHex)
		}
		if *keyHex != "" {
			return errors.New("--key applies only with --file; a stored run names a self-certifying signer")
		}
		if fs.NArg() < 1 {
			return errors.New(usage)
		}
		return verifyRun(dataDir, fs.Arg(0))
	}
	return errors.New(usage)
}

// verifyRun reads a run's stored signed record and reports every tier it satisfies:
// integrity (the events rebuild the signed Merkle root), governance (no action ran
// without admission), and ground truth (a claimed success is backed by a passing
// check). The signer is self-certifying, so a stored run needs only the durable store.
func verifyRun(dataDir, runID string) error {
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	events, err := store.Log().Read(ctx, spine.Query{Stream: runID})
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("no run found with id %q under %s", runID, dataDir)
	}
	record, err := recordFromEvents(events)
	if err != nil {
		return fmt.Errorf("run %q: %w", runID, err)
	}
	return verifyRecord(os.Stdout, "run "+runID, record, "")
}

// verifyRecordFile reads a signed record from a file and reports every tier it
// satisfies. A record whose signer is self-certifying is verified with no further
// input; one signed by another key (a published conformance vector) needs that key in
// hex via --key.
func verifyRecordFile(path, keyHex string) error {
	record, err := os.ReadFile(path) //nolint:gosec // the path is an operator-supplied record to verify
	if err != nil {
		return err
	}
	return verifyRecord(os.Stdout, path, record, keyHex)
}

// verifyRecord resolves the record's signing key and reports, tier by tier, what the
// record proves. It returns errChecksFailed if any tier is not satisfied, so the
// command exits non-zero while still printing the full report.
func verifyRecord(out io.Writer, label string, record []byte, keyHex string) error {
	keyID, err := chain.RecordKeyID(record)
	if err != nil {
		return err
	}
	pub, err := resolveKey(keyID, keyHex)
	if err != nil {
		return err
	}
	ring := chain.NewRootKeyring()
	if err := ring.Add(keyID, pub); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "%s\n", label)
	events, err := chain.VerifyRun(record, ring)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  integrity:    NOT VERIFIED: %v\n", err)
		return errChecksFailed
	}
	_, _ = fmt.Fprintf(out, "  integrity:    VERIFIED (%d events, signed by %s)\n", len(events), keyID)

	failed := false
	if gerr := chain.VerifyGovernance(events); gerr != nil {
		_, _ = fmt.Fprintf(out, "  governance:   VIOLATION: %v\n", gerr)
		failed = true
	} else {
		_, _ = fmt.Fprintln(out, "  governance:   OK (no action ran without admission)")
	}

	gt := chain.VerifyGroundTruth(events)
	switch {
	case !claimsSuccess(events):
		_, _ = fmt.Fprintln(out, "  ground-truth: not asserted (no independent check was bound)")
	case gt == nil:
		_, _ = fmt.Fprintln(out, "  ground-truth: GROUNDED (success backed by a passing check)")
	default:
		_, _ = fmt.Fprintf(out, "  ground-truth: NOT GROUNDED: %v\n", gt)
		failed = true
	}
	if failed {
		return errChecksFailed
	}
	return nil
}

// resolveKey recovers the public key a record is verified against: the supplied hex
// key when given, otherwise the self-certifying key the record names.
func resolveKey(keyID, keyHex string) (ed25519.PublicKey, error) {
	if keyHex != "" {
		raw, err := hex.DecodeString(keyHex)
		if err != nil {
			return nil, fmt.Errorf("--key is not valid hex: %w", err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("--key must be a %d-byte Ed25519 public key, got %d bytes", ed25519.PublicKeySize, len(raw))
		}
		return ed25519.PublicKey(raw), nil
	}
	pub, err := controlplane.ParsePrincipalID(keyID)
	if err != nil {
		return nil, fmt.Errorf("the record is signed by %q, which is not a self-certifying key; supply its public key with --key", keyID)
	}
	return pub, nil
}

// claimsSuccess reports whether the run records a success outcome, which is what the
// ground-truth tier applies to. A run that claims nothing needs no backing check.
func claimsSuccess(events []spine.Event) bool {
	for _, e := range events {
		if e.Type != chain.OutcomeRecorded {
			continue
		}
		if result, _ := e.Payload[chain.OutcomeResultKey].(string); result == chain.ResultSuccess {
			return true
		}
	}
	return false
}

// recordFromEvents extracts the signed record stored on a run's stream.
func recordFromEvents(events []spine.Event) ([]byte, error) {
	for _, e := range events {
		if e.Type != recordEventType {
			continue
		}
		s, ok := e.Payload["record"].(string)
		if !ok {
			return nil, errors.New("malformed record event")
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("record is not valid base64: %w", err)
		}
		return b, nil
	}
	return nil, errors.New("has no signed record; it was not sealed")
}
