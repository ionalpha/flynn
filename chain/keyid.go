package chain

import (
	"github.com/veraison/go-cose"

	"github.com/ionalpha/flynn/fault"
)

// RecordKeyID returns the key id a record's checkpoint was signed under, decoded
// from the signature's protected header without verifying the signature. A verifier
// uses it to look up (or, for a self-certifying key id, derive) the public key
// before checking the record with VerifyRun.
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

// checkpointKeyID extracts the key id from a COSE_Sign1 checkpoint's protected
// header. The key id is in the protected header, so it is covered by the signature
// and cannot be altered without detection once the signature is checked.
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
