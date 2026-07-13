package release

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The golden bundle proves the verifier accepts the one release that exists. It cannot
// prove what the verifier does with a bundle that is genuine in every respect except
// the one being tested, because nobody can re-sign the golden bundle. So this file
// mints a complete release of its own: its own certificate authority, its own
// transparency log key, its own signed entry with a real inclusion proof over a real
// Merkle tree, and its own signed checkpoint. Every check in the package then has a
// bundle that passes it, which is what makes it possible to break exactly one thing at
// a time and see which check catches it.
//
// The keys are generated in the test process and never leave it.

// noteMarker is the signed-note signature prefix: U+2014 followed by a space, written
// as an escape so the character does not appear literally in this file.
const noteMarker = "\u2014 "

// forgeAt is the moment the imaginary log recorded the imaginary entry. It is a
// constant so the certificate validity window, the entry's integration time, and the
// chain check all agree without asking the wall clock anything.
var forgeAt = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

const (
	forgeTag    = "v9.9.9"
	forgeRef    = "refs/tags/" + forgeTag
	forgeCommit = "0123456789abcdef0123456789abcdef01234567"
	forgeArt    = "flynn_linux_amd64.tar.gz"
	forgeDigest = "1111111111111111111111111111111111111111111111111111111111111111"
	forgeOrigin = "forge.example.test"
)

// forge holds the private material behind a synthetic release, so a test can re-sign
// after mutating whatever it wanted to mutate.
type forge struct {
	root    *trustRoot
	policy  policy
	caKey   *ecdsa.PrivateKey
	caCert  *x509.Certificate
	caDER   []byte
	logKey  *ecdsa.PrivateKey
	leafKey *ecdsa.PrivateKey
	leafDER []byte
	leafPEM []byte
}

// reissue mints a fresh signing certificate from the same authority, with the subject
// alternative name and identity extensions a test wants to test.
func (f *forge) reissue(t *testing.T, san string, ext map[string]string) *x509.Certificate {
	t.Helper()
	der := issueLeaf(t, f.caCert, f.caKey, &f.leafKey.PublicKey, san, ext)
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing the reissued certificate: %v", err)
	}
	return leaf
}

func forgePolicy() policy {
	p := defaultPolicy()
	p.checkpointName = forgeOrigin
	return p
}

// newForge mints a certificate authority, a signing certificate carrying this
// project's release identity, and a transparency-log key, then wires them into a trust
// root the verifier will accept.
func newForge(t *testing.T) *forge {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating the authority key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "forge authority"},
		NotBefore:             forgeAt.Add(-time.Hour),
		NotAfter:              forgeAt.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating the authority certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing the authority certificate: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating the signing key: %v", err)
	}
	p := forgePolicy()
	ext := fulcioValues(p, forgeRef, forgeCommit)
	leafDER := issueLeaf(t, caCert, caKey, &leafKey.PublicKey, ext[oidBuildSignerURI.String()], ext)

	logKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating the log key: %v", err)
	}
	logDER, err := x509.MarshalPKIXPublicKey(&logKey.PublicKey)
	if err != nil {
		t.Fatalf("marshalling the log key: %v", err)
	}
	keyID := sha256.Sum256(logDER)

	roots := x509.NewCertPool()
	roots.AddCert(caCert)

	return &forge{
		root: &trustRoot{
			roots:         roots,
			intermediates: x509.NewCertPool(),
			logKey:        &logKey.PublicKey,
			logKeyID:      keyID[:],
		},
		policy:  p,
		caKey:   caKey,
		caCert:  caCert,
		caDER:   caDER,
		logKey:  logKey,
		leafKey: leafKey,
		leafDER: leafDER,
		leafPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
	}
}

// fulcioValues is the identity the policy demands, expressed as the certificate
// extensions Fulcio would have written.
func fulcioValues(p policy, ref, commit string) map[string]string {
	signer := p.repositoryURI + "/" + p.workflowPath + "@" + ref
	return map[string]string{
		oidIssuer.String():         p.oidcIssuer,
		oidBuildSignerURI.String(): signer,
		oidRunnerEnv.String():      p.runnerEnv,
		oidSourceRepoURI.String():  p.repositoryURI,
		oidSourceRepoSHA.String():  commit,
		oidSourceRepoRef.String():  ref,
		oidSourceRepoID.String():   p.repositoryID,
		oidOwnerID.String():        p.ownerID,
		oidBuildTrigger.String():   p.buildTrigger,
	}
}

// issueLeaf mints a short-lived signing certificate carrying the given Fulcio
// extensions, with the subject alternative name Fulcio would have given it: the build
// signer URI.
func issueLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, pub any, san string, ext map[string]string) []byte {
	t.Helper()

	u, err := url.Parse(san)
	if err != nil {
		t.Fatalf("parsing the signer uri: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    forgeAt.Add(-time.Minute),
		NotAfter:     forgeAt.Add(9 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{u},
	}
	for oid, value := range ext {
		tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, derStringExtension(t, oid, value))
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, pub, caKey)
	if err != nil {
		t.Fatalf("creating the signing certificate: %v", err)
	}
	return der
}

// derStringExtension builds one Fulcio extension: a DER-encoded string, which is the
// current encoding (the pre-2023 extensions carried bare strings).
func derStringExtension(t *testing.T, oid, value string) pkix.Extension {
	t.Helper()
	id, err := parseOID(oid)
	if err != nil {
		t.Fatalf("parsing oid %q: %v", oid, err)
	}
	raw, err := asn1.Marshal(value)
	if err != nil {
		t.Fatalf("marshalling extension %s: %v", oid, err)
	}
	return pkix.Extension{Id: id, Value: raw}
}

func parseOID(s string) (asn1.ObjectIdentifier, error) {
	var out asn1.ObjectIdentifier
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// forgeStatement is the default signed payload: a SLSA provenance statement the policy
// accepts, as a map so a test can rewrite any part of it and have it re-signed.
func forgeStatement() map[string]any {
	return map[string]any{
		"_type":         statementType,
		"predicateType": predicateType,
		"subject": []any{map[string]any{
			"name":   forgeArt,
			"digest": map[string]any{"sha256": forgeDigest},
		}},
		"predicate": map[string]any{
			"buildDefinition": map[string]any{
				"buildType": buildType,
				"externalParameters": map[string]any{
					"workflow": map[string]any{
						"ref":        forgeRef,
						"repository": defaultPolicy().repositoryURI,
						"path":       defaultPolicy().workflowPath,
					},
				},
			},
		},
	}
}

// bundleJSON assembles a complete, verifiable Sigstore bundle around the given signed
// payload: DSSE signature, logged entry, signed entry timestamp, inclusion proof over a
// one-leaf tree, and a checkpoint the forged log operator signed.
func (f *forge) bundleJSON(t *testing.T, stmt map[string]any) []byte {
	t.Helper()

	payload, err := json.Marshal(stmt)
	if err != nil {
		t.Fatalf("marshalling the statement: %v", err)
	}
	sig := f.signDSSE(t, payload)
	body := f.entryBody(t, payload, sig)

	const (
		logIndex   = int64(4242)
		proofIndex = int64(0)
		treeSize   = int64(1)
	)
	// A one-leaf tree: the leaf hash is the root, and the inclusion proof is empty. The
	// proof arithmetic is the real RFC 6962 arithmetic, not a stub, so a bundle that
	// passes here passes for the same reason the published one does.
	root := logHasher.HashLeaf(body)
	promise := f.signPromise(t, body, logIndex)
	checkpoint := f.checkpoint(t, forgeOrigin, root, treeSize)

	doc := map[string]any{
		"mediaType": bundleMediaTypePrefix + ".v0.3+json",
		"verificationMaterial": map[string]any{
			"certificate": map[string]any{"rawBytes": b64(f.leafDER)},
			"tlogEntries": []any{map[string]any{
				"logIndex":         itoa(logIndex),
				"logId":            map[string]any{"keyId": b64(f.root.logKeyID)},
				"kindVersion":      map[string]any{"kind": "dsse", "version": "0.0.1"},
				"integratedTime":   itoa(forgeAt.Unix()),
				"inclusionPromise": map[string]any{"signedEntryTimestamp": b64(promise)},
				"inclusionProof": map[string]any{
					"logIndex":   itoa(proofIndex),
					"rootHash":   b64(root),
					"treeSize":   itoa(treeSize),
					"hashes":     []any{},
					"checkpoint": map[string]any{"envelope": checkpoint},
				},
				"canonicalizedBody": b64(body),
			}},
		},
		"dsseEnvelope": map[string]any{
			"payload":     b64(payload),
			"payloadType": "application/vnd.in-toto+json",
			"signatures":  []any{map[string]any{"sig": b64(sig)}},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshalling the bundle: %v", err)
	}
	return raw
}

func (f *forge) signDSSE(t *testing.T, payload []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(dssePAE("application/vnd.in-toto+json", payload))
	sig, err := ecdsa.SignASN1(rand.Reader, f.leafKey, digest[:])
	if err != nil {
		t.Fatalf("signing the envelope: %v", err)
	}
	return sig
}

// entryBody is the record the log would have stored: the payload digest, the signature,
// and the certificate that made it.
func (f *forge) entryBody(t *testing.T, payload, sig []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(payload)
	body, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"payloadHash": map[string]any{"algorithm": "sha256", "value": hex.EncodeToString(sum[:])},
			"signatures": []any{map[string]any{
				"signature": b64(sig),
				"verifier":  b64(f.leafPEM),
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshalling the logged entry: %v", err)
	}
	return body
}

// signPromise is the log's signed entry timestamp over the entry's identity.
func (f *forge) signPromise(t *testing.T, body []byte, logIndex int64) []byte {
	t.Helper()
	signed, err := json.Marshal(struct {
		Body           string `json:"body"`
		IntegratedTime int64  `json:"integratedTime"`
		LogID          string `json:"logID"`
		LogIndex       int64  `json:"logIndex"`
	}{
		Body:           b64(body),
		IntegratedTime: forgeAt.Unix(),
		LogID:          hex.EncodeToString(f.root.logKeyID),
		LogIndex:       logIndex,
	})
	if err != nil {
		t.Fatalf("marshalling the entry identity: %v", err)
	}
	digest := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, f.logKey, digest[:])
	if err != nil {
		t.Fatalf("signing the entry timestamp: %v", err)
	}
	return sig
}

// checkpoint is the log's signed note about its own tree: origin, size, root, then one
// signature line carrying the four-byte key hint and the signature over the text.
func (f *forge) checkpoint(t *testing.T, origin string, root []byte, size int64) string {
	t.Helper()
	text := origin + " - 0\n" + itoa(size) + "\n" + b64(root) + "\n"
	return text + "\n" + f.noteSignature(t, origin, text)
}

func (f *forge) noteSignature(t *testing.T, signer, text string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(text))
	sig, err := ecdsa.SignASN1(rand.Reader, f.logKey, digest[:])
	if err != nil {
		t.Fatalf("signing the checkpoint: %v", err)
	}
	blob := append(append([]byte{}, f.root.logKeyID[:4]...), sig...)
	return noteMarker + signer + " " + b64(blob) + "\n"
}

func b64(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
