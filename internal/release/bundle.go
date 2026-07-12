package release

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// maxBundleBytes caps a bundle before it is parsed. A release bundle is a few tens of
// kilobytes; anything approaching this is either broken or an attempt to make the
// verifier chew through memory before it has verified anything at all.
const maxBundleBytes = 4 << 20

// bundleMediaTypePrefix is the Sigstore bundle media type family this verifier
// understands. A bundle announcing anything else is refused rather than guessed at.
const bundleMediaTypePrefix = "application/vnd.dev.sigstore.bundle"

// bundle is the subset of the Sigstore bundle this verifier reads. The fields it does
// not read (the inclusion promise, the timestamp material) are deliberately absent:
// they are alternatives to the inclusion proof, and accepting them would let a
// signature that was never actually logged pass as one that was.
type bundle struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		Certificate struct {
			RawBytes string `json:"rawBytes"`
		} `json:"certificate"`
		TLogEntries []struct {
			LogIndex string `json:"logIndex"`
			LogID    struct {
				KeyID string `json:"keyId"`
			} `json:"logId"`
			KindVersion struct {
				Kind    string `json:"kind"`
				Version string `json:"version"`
			} `json:"kindVersion"`
			IntegratedTime   string `json:"integratedTime"`
			InclusionPromise struct {
				SignedEntryTimestamp string `json:"signedEntryTimestamp"`
			} `json:"inclusionPromise"`
			InclusionProof struct {
				LogIndex   string   `json:"logIndex"`
				RootHash   string   `json:"rootHash"`
				TreeSize   string   `json:"treeSize"`
				Hashes     []string `json:"hashes"`
				Checkpoint struct {
					Envelope string `json:"envelope"`
				} `json:"checkpoint"`
			} `json:"inclusionProof"`
			CanonicalizedBody string `json:"canonicalizedBody"`
		} `json:"tlogEntries"`
	} `json:"verificationMaterial"`
	DSSEEnvelope struct {
		Payload     string `json:"payload"`
		PayloadType string `json:"payloadType"`
		Signatures  []struct {
			Sig string `json:"sig"`
		} `json:"signatures"`
	} `json:"dsseEnvelope"`

	// leafCertificate is the signing certificate in DER, decoded once at parse.
	leafCertificate []byte
	// payload and signature are the decoded DSSE parts, held so the raw bytes that
	// were signed are the exact bytes that get verified and then read.
	payload   []byte
	signature []byte
}

// tlogEntry is one transparency-log entry with its numbers already parsed. Sigstore
// encodes 64-bit fields as JSON strings, and a verifier that silently treats an
// unparseable index as zero is a verifier with a hole in it.
type tlogEntry struct {
	logIndex       int64
	integratedTime time.Time
	keyID          []byte
	body           []byte
	promise        []byte
	proofIndex     int64
	treeSize       int64
	rootHash       []byte
	hashes         [][]byte
	checkpoint     string
}

func decodeBundle(raw []byte) (*bundle, error) {
	if len(raw) == 0 {
		return nil, fault.New(fault.Terminal, CodeBundleDecode, "the release bundle is empty")
	}
	if len(raw) > maxBundleBytes {
		return nil, fault.New(fault.Terminal, CodeBundleDecode,
			fmt.Sprintf("the release bundle is %d bytes, over the %d-byte ceiling", len(raw), maxBundleBytes))
	}

	var b bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeBundleDecode, err)
	}
	if !hasPrefix(b.MediaType, bundleMediaTypePrefix) {
		return nil, fault.New(fault.Terminal, CodeBundleDecode,
			"the release bundle announces media type "+b.MediaType+", which this verifier does not understand")
	}

	var err error
	if b.leafCertificate, err = decodeB64(b.VerificationMaterial.Certificate.RawBytes, "signing certificate"); err != nil {
		return nil, err
	}
	if b.payload, err = decodeB64(b.DSSEEnvelope.Payload, "signed payload"); err != nil {
		return nil, err
	}
	if len(b.DSSEEnvelope.Signatures) != 1 {
		return nil, fault.New(fault.Terminal, CodeBundleDecode,
			fmt.Sprintf("the release bundle carries %d signatures; exactly one is expected", len(b.DSSEEnvelope.Signatures)))
	}
	if b.signature, err = decodeB64(b.DSSEEnvelope.Signatures[0].Sig, "signature"); err != nil {
		return nil, err
	}
	return &b, nil
}

// soleTLogEntry returns the bundle's single transparency-log entry. More than one
// entry would mean more than one answer to "was this logged", and picking one of
// several is how a verifier gets talked into believing the wrong one.
func (b *bundle) soleTLogEntry() (tlogEntry, error) {
	if n := len(b.VerificationMaterial.TLogEntries); n != 1 {
		return tlogEntry{}, fault.New(fault.Terminal, CodeTransparency,
			fmt.Sprintf("the release bundle carries %d transparency-log entries; exactly one is expected", n))
	}
	e := b.VerificationMaterial.TLogEntries[0]

	// The entry kind decides how the log hashed what it stored. This verifier binds a
	// dsse entry to the envelope it saw; it cannot make that argument about a kind it
	// does not know, so it refuses rather than skip the binding.
	if e.KindVersion.Kind != "dsse" || e.KindVersion.Version != "0.0.1" {
		return tlogEntry{}, fault.New(fault.Terminal, CodeTransparency,
			"the transparency-log entry is a "+e.KindVersion.Kind+"/"+e.KindVersion.Version+
				" entry; this verifier only knows how to read dsse/0.0.1, and it will not guess at the meaning of one it does not know")
	}

	var (
		out tlogEntry
		err error
	)
	if out.logIndex, err = parseInt(e.LogIndex, "log index"); err != nil {
		return tlogEntry{}, err
	}
	secs, err := parseInt(e.IntegratedTime, "integrated time")
	if err != nil {
		return tlogEntry{}, err
	}
	if secs <= 0 {
		return tlogEntry{}, fault.New(fault.Terminal, CodeTransparency, "the transparency-log entry has no integration time")
	}
	out.integratedTime = time.Unix(secs, 0).UTC()

	if out.keyID, err = decodeB64(e.LogID.KeyID, "log key id"); err != nil {
		return tlogEntry{}, err
	}
	if out.body, err = decodeB64(e.CanonicalizedBody, "logged entry body"); err != nil {
		return tlogEntry{}, err
	}
	if out.promise, err = decodeB64(e.InclusionPromise.SignedEntryTimestamp, "signed entry timestamp"); err != nil {
		return tlogEntry{}, err
	}
	if out.proofIndex, err = parseInt(e.InclusionProof.LogIndex, "inclusion proof index"); err != nil {
		return tlogEntry{}, err
	}
	// The two indices are not the same number and must not be checked against each
	// other: logIndex counts across the whole log, while the inclusion proof's index is
	// a position within the shard whose tree the proof is computed over. Each is
	// authenticated in its own way, the first by the log's signed entry timestamp and
	// the second by the proof itself reconstructing a signed root.
	if out.treeSize, err = parseInt(e.InclusionProof.TreeSize, "inclusion proof tree size"); err != nil {
		return tlogEntry{}, err
	}
	if out.rootHash, err = decodeB64(e.InclusionProof.RootHash, "inclusion proof root hash"); err != nil {
		return tlogEntry{}, err
	}
	for i, h := range e.InclusionProof.Hashes {
		d, err := decodeB64(h, fmt.Sprintf("inclusion proof hash %d", i))
		if err != nil {
			return tlogEntry{}, err
		}
		out.hashes = append(out.hashes, d)
	}
	out.checkpoint = e.InclusionProof.Checkpoint.Envelope
	if out.checkpoint == "" {
		return tlogEntry{}, fault.New(fault.Terminal, CodeTransparency,
			"the transparency-log entry carries no signed checkpoint, so its root hash is unattested")
	}
	return out, nil
}

// verifyDSSESignature checks the envelope signature against the certificate's key,
// over the DSSE pre-authentication encoding. The PAE is what makes the signature
// unambiguous: it commits to the payload type and to both lengths, so a payload
// cannot be reinterpreted as a different type or spliced with the type string.
func (b *bundle) verifyDSSESignature(leaf *x509.Certificate) error {
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fault.New(fault.Terminal, CodeSignature,
			"the signing certificate carries a non-ECDSA key, which this verifier does not accept")
	}

	pae := dssePAE(b.DSSEEnvelope.PayloadType, b.payload)

	// The digest follows the key's curve, as ECDSA is defined: a P-384 key signs a
	// SHA-384 digest. Hard-coding SHA-256 would silently fail on a rotated key type.
	var digest []byte
	switch key.Curve.Params().BitSize {
	case 256:
		h := sha256.Sum256(pae)
		digest = h[:]
	case 384:
		h := sha512.Sum384(pae)
		digest = h[:]
	case 521:
		h := sha512.Sum512(pae)
		digest = h[:]
	default:
		return fault.New(fault.Terminal, CodeSignature,
			fmt.Sprintf("the signing key uses an unsupported %d-bit curve", key.Curve.Params().BitSize))
	}

	if !ecdsa.VerifyASN1(key, digest, b.signature) {
		return fault.New(fault.Terminal, CodeSignature,
			"the release signature does not verify against the signing certificate")
	}
	return nil
}

// dssePAE builds the DSSE pre-authentication encoding, per the DSSE specification.
func dssePAE(payloadType string, payload []byte) []byte {
	out := make([]byte, 0, len(payloadType)+len(payload)+32)
	out = append(out, "DSSEv1 "...)
	out = strconv.AppendInt(out, int64(len(payloadType)), 10)
	out = append(out, ' ')
	out = append(out, payloadType...)
	out = append(out, ' ')
	out = strconv.AppendInt(out, int64(len(payload)), 10)
	out = append(out, ' ')
	out = append(out, payload...)
	return out
}

func decodeB64(s, what string) ([]byte, error) {
	if s == "" {
		return nil, fault.New(fault.Terminal, CodeBundleDecode, "the release bundle carries no "+what)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeBundleDecode, fmt.Errorf("decoding the %s: %w", what, err))
	}
	return raw, nil
}

func parseInt(s, what string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fault.Wrap(fault.Terminal, CodeBundleDecode, fmt.Errorf("reading the %s: %w", what, err))
	}
	return n, nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
