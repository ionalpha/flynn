package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
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

// sealRunFromStore seals a run's current durable stream into a signed record stored on
// the stream, without a live recorder. It reads the run's events, folds every one
// except a prior seal into a fresh record builder, signs the Merkle head, and appends
// the record. Reading the durable stream (rather than a recorder accumulated in memory)
// lets a session seal a run in-process at any point, including one continued from a
// resumed conversation whose earlier events predate this process. Re-sealing re-folds
// the same events (skipping the earlier record event, which is not part of the chain it
// attests) and appends a fresh record.
func sealRunFromStore(ctx context.Context, store *sqlite.Store, runID string, signer chain.RootSigner) error {
	events, err := store.Log().Read(ctx, spine.Query{Stream: runID})
	if err != nil {
		return err
	}
	b := chain.NewBuilder(runID)
	folded := 0
	for _, e := range events {
		if e.Type == recordEventType {
			continue
		}
		if err := b.Add(e); err != nil {
			return err
		}
		folded++
	}
	if folded == 0 {
		return fmt.Errorf("run %q has no events to seal", runID)
	}
	sealed, err := b.Seal(signer)
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

// verifyStoredRun verifies a sealed run already open in store, writing its per-tier
// report to out. It is the in-process counterpart of verifyRun, which opens its own
// store and writes to stdout: a session that holds the store open verifies its own run
// without reopening. It reports errChecksFailed if a tier fails, or a plain error when
// the run carries no sealed record yet.
func verifyStoredRun(ctx context.Context, out io.Writer, store *sqlite.Store, runID string) error {
	events, err := store.Log().Read(ctx, spine.Query{Stream: runID})
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("no run found with id %q", runID)
	}
	record, err := recordFromEvents(events)
	if err != nil {
		return err
	}
	return verifyRecord(out, "run "+runID, record, "")
}

// errChecksFailed reports that a record did not pass every check. The per-tier detail
// is written to stdout; this is the concise error that sets a non-zero exit code so
// the command can gate a script.
var errChecksFailed = errors.New("record did not pass all checks")

// dispatchSpine handles the spine subcommands.
func dispatchSpine(args []string, dataDir string) error {
	const usage = "usage: flynn spine verify [--file <path> [--key <hex>]] <run-id>\n" +
		"       flynn spine export [--out <path>] <run-id>"
	switch {
	case len(args) >= 1 && args[0] == "verify":
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
	case len(args) >= 1 && args[0] == "export":
		fs := flag.NewFlagSet("spine export", flag.ContinueOnError)
		out := fs.String("out", "", "write the record to this file (default <run-id>.flynnrecord in the current directory)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return errors.New(usage)
		}
		return exportRun(dataDir, fs.Arg(0), *out)
	}
	return errors.New(usage)
}

// exportRun writes a sealed run's portable record to a file, so it can be verified by a
// third party (or with `flynn spine verify --file`) without the durable store. It opens
// the store, extracts the signed record the run carries, and writes it to path, defaulting
// to <run-id>.flynnrecord in the current directory. A run that was never sealed is
// reported rather than writing an empty file.
func exportRun(dataDir, runID, path string) error {
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if path == "" {
		path = runID + ".flynnrecord"
	}
	if err := exportRecord(ctx, store, runID, path); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "exported record for run %s to %s; verify with: flynn spine verify --file %s\n", runID, path, path)
	return nil
}

// exportRecord extracts the signed record stored on a run's stream and writes its portable
// bytes to path. The bytes are the same canonical artifact `chain.VerifyRun` checks, so
// the file is independently verifiable. It reports a run with no sealed record rather than
// writing a partial file.
func exportRecord(ctx context.Context, store *sqlite.Store, runID, path string) error {
	events, err := store.Log().Read(ctx, spine.Query{Stream: runID})
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return fmt.Errorf("no run found with id %q", runID)
	}
	record, err := recordFromEvents(events)
	if err != nil {
		return fmt.Errorf("run %q %w", runID, err)
	}
	return os.WriteFile(path, record, 0o600)
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
		// No per-run sealed record: the stream may instead be a durably checkpointed
		// one (the served path), verifiable from its latest signed checkpoint.
		return verifyCheckpointedStream(os.Stdout, store, runID)
	}
	return verifyRecord(os.Stdout, "run "+runID, record, "")
}

// verifyCheckpointedStream verifies a stream that carries no single sealed record but a
// durable, periodically checkpointed Merkle log: the events rebuild the latest signed
// checkpoint's root (integrity), then the same governance and ground-truth tiers are
// reported. The checkpoint's key is self-certifying, so a stored stream needs only the
// durable store.
func verifyCheckpointedStream(out io.Writer, store *sqlite.Store, stream string) error {
	ctx := context.Background()
	_, cose, ok, err := store.LatestCheckpoint(ctx, stream)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("stream %q has no signed record and no checkpoint; it was not sealed", stream)
	}
	keyID, err := chain.CheckpointKeyID(cose)
	if err != nil {
		return err
	}
	pub, err := resolveKey(keyID, "")
	if err != nil {
		return err
	}
	ring := chain.NewRootKeyring()
	if err := ring.Add(keyID, pub); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "stream %s\n", stream)
	cp, err := chain.VerifyCheckpoint(cose, ring)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  integrity:    NOT VERIFIED: %v\n", err)
		return errChecksFailed
	}
	// Fold the first cp.Size events and require they rebuild the signed root.
	limit := 0 // 0 means no limit; the fold below stops at cp.Size regardless.
	if cp.Size <= math.MaxInt {
		limit = int(cp.Size)
	}
	events, err := store.Log().Read(ctx, spine.Query{Stream: stream, Limit: limit})
	if err != nil {
		return err
	}
	tree := chain.NewTree()
	covered := make([]spine.Event, 0, len(events))
	for _, e := range events {
		if uint64(len(covered)) >= cp.Size {
			break
		}
		cb, cerr := chain.CanonicalBytes(e)
		if cerr != nil {
			_, _ = fmt.Fprintf(out, "  integrity:    NOT VERIFIED: %v\n", cerr)
			return errChecksFailed
		}
		if aerr := tree.Append(cb); aerr != nil {
			_, _ = fmt.Fprintf(out, "  integrity:    NOT VERIFIED: %v\n", aerr)
			return errChecksFailed
		}
		covered = append(covered, e)
	}
	root, err := tree.Root()
	if err != nil {
		return err
	}
	if uint64(len(covered)) != cp.Size || !bytes.Equal(root, cp.RootHash) {
		_, _ = fmt.Fprintf(out, "  integrity:    NOT VERIFIED: events do not reproduce the signed checkpoint root\n")
		return errChecksFailed
	}
	_, _ = fmt.Fprintf(out, "  integrity:    VERIFIED (%d events, checkpoint signed by %s)\n", cp.Size, keyID)
	if reportGovernanceAndGroundTruth(out, covered) {
		return errChecksFailed
	}
	return nil
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

	if reportGovernanceAndGroundTruth(out, events) {
		return errChecksFailed
	}
	return nil
}

// reportGovernanceAndGroundTruth reports the governance and ground-truth tiers over a
// verified event sequence and returns whether either failed. It is shared by the sealed
// record and durable checkpoint verify paths so both report the same tiers identically.
func reportGovernanceAndGroundTruth(out io.Writer, events []spine.Event) (failed bool) {
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
	return failed
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
