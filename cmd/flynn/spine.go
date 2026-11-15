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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/storage/sqlite"
)

// recordEventType is the event type a sealed run's signed record is stored under on
// its own stream, so the durable store alone can verify the run.
const recordEventType = "spine.record"

// runSigner loads or first creates the instance identity in the vault under dataDir.
// The key id is the identity's self-certifying principal id, so a verifier recovers the
// public key from a record with no shared state.
func runSigner(ctx context.Context, dataDir string) (chain.RootSigner, error) {
	v := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase))
	id, err := controlplane.LoadOrCreateIdentity(ctx, v, "")
	if err != nil {
		return nil, err
	}
	return chain.NewEd25519RootSigner(id.ID(), ed25519.NewKeyFromSeed(id.Seed()))
}

// defaultSnapshotEvery is the number of resource mutations between verified
// checkpoints; FLYNN_SNAPSHOT_EVERY overrides it and 0 disables them. Each snapshot
// folds the stream prefix once, so the cadence trades write cost against how far a
// rebuild folds past the last checkpoint.
const defaultSnapshotEvery = 256

// snapshotOptions enables snapshots sealed under the run key (checkpoint-bound,
// COSE-signed) and verified against its public key on rebuild. With no signer it
// returns nothing, because an unverified snapshot would be trusted blindly; the store
// then folds from the log alone.
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

// snapshotEvery reads FLYNN_SNAPSHOT_EVERY, defaulting to defaultSnapshotEvery.
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

// defaultCheckpointEvery is the number of events between signed checkpoints of a served
// stream's Merkle log; FLYNN_CHECKPOINT_EVERY overrides it. A checkpoint flushes tiles
// and signs the head, so the cadence trades write cost against how far a restart
// re-folds.
const defaultCheckpointEvery = 256

// checkpointEvery reads FLYNN_CHECKPOINT_EVERY, defaulting to defaultCheckpointEvery.
// Zero checkpoints only on an explicit call.
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

// sealRun stores the signed record as one event on the run's stream. It appends through
// the underlying log, bypassing the recorder, so the record is not folded into the
// chain it attests.
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

// Results a run that did not converge records. None is chain.ResultSuccess, so the
// ground-truth tier asks no backing check of them.
const (
	resultBudget    = "budget_exceeded"
	resultKilled    = "killed"
	resultCancelled = "cancelled"
	resultStalled   = "stalled"
	resultError     = "error"

	// outcomeReasonKey holds, on a recorded outcome, the error the run ended with.
	outcomeReasonKey = "reason"
)

// stoppedOutcome maps a run's error to its record's result. A typed fault wins;
// otherwise a kill and a budget stop are told apart by the message the stopping side
// wrote, since the stream flattens a stall into text.
func stoppedOutcome(runErr error) string {
	msg := runErr.Error()
	switch {
	case fault.Classify(runErr) == fault.BudgetExceeded, strings.Contains(msg, string(fault.BudgetExceeded)):
		return resultBudget
	case strings.Contains(msg, goal.KillMessage(goal.Kill{})):
		return resultKilled
	case errors.Is(runErr, context.Canceled):
		return resultCancelled
	case strings.HasPrefix(msg, "goal stalled"):
		return resultStalled
	}
	return resultError
}

// sealReported seals and reports the result on out. A sealing failure never fails the
// run, whose result is already decided; a missing record is visible to whoever asks.
func sealReported(ctx context.Context, out io.Writer, store *sqlite.Store, rec *chain.RecordingLog, runID string, signer chain.RootSigner) {
	if serr := sealRun(ctx, store, rec, runID, signer); serr != nil {
		_, _ = fmt.Fprintf(out, "  (run not sealed: %v)\n", serr)
		return
	}
	_, _ = fmt.Fprintf(out, "  run sealed; verify with: flynn spine verify %s\n", runID)
}

// sealStopped records how a non-converged run ended, through the recorder so the
// outcome is inside the signed chain, then seals. The caller returns runErr unchanged.
//
// Stalls, budget stops, kills and provider failures are the runs people check later.
// The seal uses a context detached from ctx, because an interrupt cancels ctx and those
// are the runs whose records must still be written.
func sealStopped(ctx context.Context, out io.Writer, store *sqlite.Store, rec *chain.RecordingLog, runID string, signer chain.RootSigner, runErr error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := rec.Append(ctx, spine.AppendInput{
		Stream: runID,
		Type:   chain.OutcomeRecorded,
		Actor:  spine.ActorSystem,
		Payload: map[string]any{
			chain.OutcomeResultKey: stoppedOutcome(runErr),
			outcomeReasonKey:       runErr.Error(),
		},
	}); err != nil {
		_, _ = fmt.Fprintf(out, "  (outcome not recorded: %v)\n", err)
	}
	sealReported(ctx, out, store, rec, runID, signer)
}

// sealResumed seals a `flynn resume` run from its durable stream, since earlier events
// came from another process. A stopped run records its ending first, as in
// [sealStopped]. An interrupt is left unsealed: Ctrl-C detaches and the run may resume,
// which would leave a stopped outcome inside a run that went on.
func sealResumed(ctx context.Context, out io.Writer, store *sqlite.Store, runID string, signer chain.RootSigner, runErr error) {
	if errors.Is(runErr, context.Canceled) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if runErr != nil {
		if _, err := store.Log().Append(ctx, spine.AppendInput{
			Stream: runID,
			Type:   chain.OutcomeRecorded,
			Actor:  spine.ActorSystem,
			Payload: map[string]any{
				chain.OutcomeResultKey: stoppedOutcome(runErr),
				outcomeReasonKey:       runErr.Error(),
			},
		}); err != nil {
			_, _ = fmt.Fprintf(out, "  (outcome not recorded: %v)\n", err)
		}
	}
	if err := sealRunFromStore(ctx, store, runID, signer); err != nil {
		_, _ = fmt.Fprintf(out, "  (run not sealed: %v)\n", err)
		return
	}
	_, _ = fmt.Fprintf(out, "  run sealed; verify with: flynn spine verify %s\n", runID)
}

// sealRunFromStore seals a run's durable stream without a live recorder: it folds every
// event except a prior seal, signs the Merkle head and appends the record. Reading the
// store lets a session seal at any point, including a run whose earlier events predate
// this process. A re-seal folds the same events and appends a fresh record.
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

// verifyStoredRun is verifyRun for a store a session already holds open. It returns
// errChecksFailed if a tier fails, or an error when the run has no sealed record.
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

// errChecksFailed makes the command exit non-zero so it can gate a script; the per-tier
// detail is already on stdout.
var errChecksFailed = errors.New("record did not pass all checks")

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

// exportRun writes a sealed run's portable record (default <run-id>.flynnrecord in the
// current directory), so a third party or `flynn spine verify --file` can check it
// without the store. An unsealed run is reported and no file is written.
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

// exportRecord writes the canonical bytes `chain.VerifyRun` checks, so the file verifies
// independently. A run with no sealed record is reported, never written partially.
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
	return fsatomic.WriteFile(path, record, 0o600)
}

// verifyRun reports every tier a stored record satisfies: integrity (events rebuild the
// signed Merkle root), governance (no action ran unadmitted) and ground truth (a claimed
// success is backed by a passing check). The signer is self-certifying, so only the
// store is needed.
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
		// No sealed record: the stream may be a checkpointed one from the served path.
		return verifyCheckpointedStream(os.Stdout, store, runID)
	}
	return verifyRecord(os.Stdout, "run "+runID, record, "")
}

// verifyCheckpointedStream verifies a stream with a periodically checkpointed Merkle log
// in place of a sealed record: the events must rebuild the latest checkpoint's root, then
// governance and ground truth are reported. The checkpoint key is self-certifying.
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

// verifyRecordFile verifies a record file. A self-certifying record needs nothing more;
// one signed by another key (a published conformance vector) needs it in hex via --key.
func verifyRecordFile(path, keyHex string) error {
	record, err := os.ReadFile(path) //nolint:gosec // the path is an operator-supplied record to verify
	if err != nil {
		return err
	}
	return verifyRecord(os.Stdout, path, record, keyHex)
}

// verifyRecord prints the full per-tier report and returns errChecksFailed if any tier
// fails.
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

// reportGovernanceAndGroundTruth reports those two tiers for both verify paths, so they
// read identically, and returns whether either failed.
func reportGovernanceAndGroundTruth(out io.Writer, events []spine.Event) (failed bool) {
	if gerr := chain.VerifyGovernance(events); gerr != nil {
		_, _ = fmt.Fprintf(out, "  governance:   VIOLATION: %v\n", gerr)
		failed = true
	} else {
		_, _ = fmt.Fprintln(out, "  governance:   OK (no action ran without admission)")
	}

	// Integrity says the record is faithful, not that the run finished, so a stopped run
	// says so here.
	if result, reason, ok := stoppedBy(events); ok {
		_, _ = fmt.Fprintf(out, "  outcome:      %s (%s)\n", result, reason)
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

	if reportProvenance(out, events) {
		failed = true
	}
	return failed
}

// reportProvenance prints the provenance tier of an external-agent run. A native run
// has no declaration and prints nothing. An external run states that its effects are
// enforced while the harness's reasoning is unobserved and the run cannot be replayed,
// so the record never claims a native run's integrity.
//
// It returns whether its one check failed: the declaration's attested count must match
// the harness events the record carries. Otherwise the record's most quoted number, "N
// events attested", could not be inspected.
func reportProvenance(out io.Writer, events []spine.Event) bool {
	attested := chain.AttestedEventsOf(events)
	if aerr := chain.VerifyAttestation(events); aerr != nil {
		_, _ = fmt.Fprintf(out, "  attestation:  VIOLATION: %v\n", aerr)
		return true
	}
	p, ok := chain.ProvenanceOf(events)
	if !ok {
		return false
	}
	replay := "run is non-replayable (episode-level granularity)"
	if p.Replayable {
		replay = "run is replayable"
	}
	_, _ = fmt.Fprintf(out, "  provenance:   effects %s, reasoning %s (external harness: %s); %s\n",
		strings.ToUpper(p.Effects), strings.ToUpper(p.Reasoning), p.Harness, replay)
	// The harness's own account, carried but not vouched for. It prints only when nonzero.
	// The breakdown by kind separates what the harness says it did from the enforced
	// effects above; the record proves only that it said them.
	if p.AttestedEvents > 0 {
		_, _ = fmt.Fprintf(out, "                %d event(s) %s by the harness (its own account, unverified): %s\n",
			p.AttestedEvents, strings.ToUpper(chain.TierAttested), attestedKinds(attested))
	}
	// The harness's prompt outranks the run's, so the behavioural contract is measured and
	// not imposed. When the harness ignored it, the same signature covers more unobserved
	// behaviour, and the reader is told.
	if len(p.Drift) > 0 {
		names := make([]string, 0, len(p.Drift))
		for name := range p.Drift {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%s x%d", name, p.Drift[name]))
		}
		_, _ = fmt.Fprintf(out, "                contract drift: %s\n", strings.Join(parts, ", "))
	}
	if p.NativeToolRate > 0 {
		_, _ = fmt.Fprintf(out, "                %.0f%% of the harness's tool attempts used its own tools (unobserved reads)\n",
			p.NativeToolRate*100)
	}
	return false
}

// attestedKinds summarizes events by kind ("text x3, bridge_call x2") in a stable order,
// so two verifications read the same.
func attestedKinds(events []chain.AttestedEvent) string {
	counts := map[string]int{}
	for _, e := range events {
		counts[e.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		parts = append(parts, fmt.Sprintf("%s x%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// resolveKey returns the supplied hex key, or else the self-certifying key the record
// names.
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

// claimsSuccess reports a recorded success outcome, the only claim the ground-truth tier
// checks.
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

// stoppedBy returns the last non-success outcome, which is how a non-converged run
// records its end.
func stoppedBy(events []spine.Event) (result, reason string, ok bool) {
	for _, e := range events {
		if e.Type != chain.OutcomeRecorded {
			continue
		}
		r, _ := e.Payload[chain.OutcomeResultKey].(string)
		if r == "" || r == chain.ResultSuccess {
			continue
		}
		reason, _ = e.Payload[outcomeReasonKey].(string)
		result, ok = r, true
	}
	return result, reason, ok
}

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
