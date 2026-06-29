package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/storage/sqlite"
	"github.com/ionalpha/flynn/vault"
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

// dispatchSpine handles the spine subcommands.
func dispatchSpine(args []string, dataDir string) error {
	if len(args) >= 1 && args[0] == "verify" {
		if len(args) < 2 {
			return errors.New("usage: flynn spine verify <run-id>")
		}
		return verifyRun(dataDir, args[1])
	}
	return errors.New("usage: flynn spine verify <run-id>")
}

// verifyRun reads a run's stored signed record and checks it: the record's events
// rebuild the signed Merkle root and the signature is valid under the key the record
// names. The key id is self-certifying, so verification needs only the durable store
// and the record itself.
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

	keyID, err := chain.RecordKeyID(record)
	if err != nil {
		return err
	}
	pub, err := controlplane.ParsePrincipalID(keyID)
	if err != nil {
		return fmt.Errorf("run %q names an unrecognizable signer: %w", runID, err)
	}
	ring := chain.NewRootKeyring()
	if err := ring.Add(keyID, pub); err != nil {
		return err
	}

	verified, err := chain.VerifyRun(record, ring)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "run %s: NOT VERIFIED: %v\n", runID, err)
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "run %s: VERIFIED, %d events, signed by %s\n", runID, len(verified), keyID)
	return nil
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
