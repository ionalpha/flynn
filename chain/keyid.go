package chain

import (
	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/fault"
)

// RecordKeyID returns the key id from the checkpoint's protected header without
// verifying the signature, so a verifier can look up or derive the public key
// before [VerifyRun].
func RecordKeyID(record []byte) (string, error) {
	// Decode only the checkpoint: the full wire struct would materialize every
	// event byte string just to read one header.
	var w struct {
		Checkpoint []byte `cbor:"checkpoint"`
	}
	if err := canonicalDec.Unmarshal(record, &w); err != nil {
		return "", fault.Wrap(fault.Terminal, CodeRecordDecode, err)
	}
	return checkpointKeyID(w.Checkpoint)
}

// CheckpointKeyID is [RecordKeyID] for a standalone checkpoint, before
// [VerifyCheckpoint].
func CheckpointKeyID(coseBytes []byte) (string, error) {
	return checkpointKeyID(coseBytes)
}

// checkpointKeyID reads the protected header, which the signature covers once
// verified.
func checkpointKeyID(coseBytes []byte) (string, error) {
	var msg cose.Sign1Message
	if err := msg.UnmarshalCBOR(coseBytes); err != nil {
		return "", fault.Wrap(fault.Terminal, CodeSignatureInvalid, err)
	}
	kid, _ := msg.Headers.Protected[cose.HeaderLabelKeyID].([]byte)
	if len(kid) == 0 {
		return "", fault.New(fault.Terminal, CodeUnknownKey, "chain: checkpoint has no key id")
	}
	return string(kid), nil
}
