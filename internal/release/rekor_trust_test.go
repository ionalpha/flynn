package release

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// forged returns a decoded synthetic bundle and its transparency-log entry, so the log
// checks can be exercised one at a time on material that is otherwise genuine.
func forged(t *testing.T) (*forge, *bundle, tlogEntry) {
	t.Helper()
	f := newForge(t)
	b, err := decodeBundle(f.bundleJSON(t, forgeStatement()))
	if err != nil {
		t.Fatalf("decodeBundle: %v", err)
	}
	e, err := b.soleTLogEntry()
	if err != nil {
		t.Fatalf("soleTLogEntry: %v", err)
	}
	return f, b, e
}

// The logged entry is what ties the proof to this envelope. Break any of the three
// things it commits to (the payload, the signature, the certificate) and the proof
// stops being a proof about us: it becomes a proof about somebody else's entry that
// happens to be in the same log.
func TestBindEntryToEnvelopeRejectsAnEntryAboutSomethingElse(t *testing.T) {
	f, b, e := forged(t)

	// A second, unrelated release: its signature and certificate are genuine, and they
	// are not the ones in this bundle.
	other := newForge(t)

	rewrite := func(t *testing.T, mutate func(body map[string]any)) []byte {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal(e.body, &body); err != nil {
			t.Fatalf("unmarshalling the logged entry: %v", err)
		}
		mutate(body)
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling the logged entry: %v", err)
		}
		return raw
	}
	spec := func(body map[string]any) map[string]any { return body["spec"].(map[string]any) }
	sigs := func(body map[string]any) map[string]any {
		return spec(body)["signatures"].([]any)[0].(map[string]any)
	}

	cases := []struct {
		name     string
		body     func(t *testing.T) []byte
		contains string
	}{
		{
			name:     "a body that is not a logged entry at all",
			body:     func(*testing.T) []byte { return []byte("not json") },
			contains: "reading the logged entry",
		},
		{
			name: "an entry hashed with an algorithm this verifier does not accept",
			body: func(t *testing.T) []byte {
				return rewrite(t, func(body map[string]any) {
					spec(body)["payloadHash"] = map[string]any{"algorithm": "sha1", "value": "00"}
				})
			},
			contains: "hashes its payload with sha1",
		},
		{
			name: "an entry recording a different payload",
			body: func(t *testing.T) []byte {
				return rewrite(t, func(body map[string]any) {
					spec(body)["payloadHash"].(map[string]any)["value"] = strings.Repeat("ab", 32)
				})
			},
			contains: "records a different payload",
		},
		{
			name: "an entry recording no signature",
			body: func(t *testing.T) []byte {
				return rewrite(t, func(body map[string]any) {
					spec(body)["signatures"] = []any{}
				})
			},
			contains: "records 0 signatures",
		},
		{
			name: "an entry whose signature is not base64",
			body: func(t *testing.T) []byte {
				return rewrite(t, func(body map[string]any) {
					sigs(body)["signature"] = "!!!"
				})
			},
			contains: "reading the logged signature",
		},
		{
			name: "an entry recording a different signature",
			body: func(t *testing.T) []byte {
				return rewrite(t, func(body map[string]any) {
					sigs(body)["signature"] = base64.StdEncoding.EncodeToString([]byte("some other signature"))
				})
			},
			contains: "records a different signature",
		},
		{
			name: "an entry whose certificate is not base64",
			body: func(t *testing.T) []byte {
				return rewrite(t, func(body map[string]any) {
					sigs(body)["verifier"] = "!!!"
				})
			},
			contains: "reading the logged certificate",
		},
		{
			name: "an entry recording a different signing certificate",
			body: func(t *testing.T) []byte {
				return rewrite(t, func(body map[string]any) {
					sigs(body)["verifier"] = base64.StdEncoding.EncodeToString(other.leafPEM)
				})
			},
			contains: "records a different signing certificate",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantFault(t, f.root.bindEntryToEnvelope(tc.body(t), b), CodeTransparency, tc.contains)
		})
	}

	t.Run("the entry the log really recorded", func(t *testing.T) {
		if err := f.root.bindEntryToEnvelope(e.body, b); err != nil {
			t.Fatalf("the genuine logged entry was not bound to its envelope: %v", err)
		}
	})
}

// The integration time is an attacker-chosen number until the log's signature over it
// is checked, and it is the number that decides whether an expired certificate passes.
func TestVerifyEntryTimestampRejectsAnUnsignedIntegrationTime(t *testing.T) {
	f, _, e := forged(t)

	if err := f.root.verifyEntryTimestamp(e); err != nil {
		t.Fatalf("the genuine signed entry timestamp was rejected: %v", err)
	}

	cases := map[string]func(e *tlogEntry){
		"the integration time moved":     func(e *tlogEntry) { e.integratedTime = e.integratedTime.Add(time.Second) },
		"the entry moved in the log":     func(e *tlogEntry) { e.logIndex++ },
		"the body swapped under the sig": func(e *tlogEntry) { e.body = append(e.body, ' ') },
		"the timestamp replaced":         func(e *tlogEntry) { e.promise = []byte{0x30, 0x00} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := e
			mutate(&bad)
			wantFault(t, f.root.verifyEntryTimestamp(bad), CodeTransparency,
				"did not sign this entry at the time the bundle claims")
		})
	}
}

// An inclusion proof for a position that does not exist proves nothing, and the proof
// arithmetic must not be asked to reason about one.
func TestVerifyInclusionRejectsImpossiblePositions(t *testing.T) {
	f, b, e := forged(t)

	cases := map[string]func(e *tlogEntry){
		"an index past the end of the tree": func(e *tlogEntry) { e.proofIndex = e.treeSize },
		"a negative index":                  func(e *tlogEntry) { e.proofIndex = -1 },
		"an empty tree":                     func(e *tlogEntry) { e.treeSize = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := e
			mutate(&bad)
			wantFault(t, f.root.verifyInclusion(bad, b, f.policy.checkpointName), CodeTransparency,
				"is not a position that exists")
		})
	}

	t.Run("a proof that does not reconstruct the root", func(t *testing.T) {
		bad := e
		bad.hashes = [][]byte{make([]byte, sha256.Size)}
		wantFault(t, f.root.verifyInclusion(bad, b, f.policy.checkpointName), CodeTransparency,
			"does not reconstruct the log's root")
	})

	// A proof about an entry that is in the log but is not a record of this envelope: the
	// binding check has to run before the arithmetic, or the arithmetic proves the wrong
	// thing.
	t.Run("an entry that is not a record of this envelope", func(t *testing.T) {
		bad := e
		bad.body = []byte("not a logged entry")
		wantFault(t, f.root.verifyInclusion(bad, b, f.policy.checkpointName), CodeTransparency,
			"reading the logged entry")
	})

	// An entry the log never signed at the time the bundle claims: the integration time is
	// what the certificate window is checked against, so it is checked before the proof.
	t.Run("an entry with an unsigned integration time", func(t *testing.T) {
		bad := e
		bad.promise = []byte{0x30, 0x00}
		wantFault(t, f.root.verifyInclusion(bad, b, f.policy.checkpointName), CodeTransparency,
			"did not sign this entry at the time the bundle claims")
	})

	t.Run("the entry the log really recorded", func(t *testing.T) {
		if err := f.root.verifyInclusion(e, b, f.policy.checkpointName); err != nil {
			t.Fatalf("a genuine inclusion proof was rejected: %v", err)
		}
	})
}

// The checkpoint is the log operator's signed statement about its own tree. Everything
// the inclusion proof shows is conditional on this note being genuine, being about the
// tree the proof was computed over, and coming from the log this binary pins.
func TestVerifyCheckpoint(t *testing.T) {
	f, _, e := forged(t)
	name := f.policy.checkpointName

	text, _, ok := splitNote(e.checkpoint)
	if !ok {
		t.Fatal("the forged checkpoint is not a signed note")
	}
	goodSig := f.noteSignature(t, name, text)

	// A signature from a log that is not this one, over the same text. The verifier has
	// to walk past it rather than stop at it.
	decoy := noteMarker + "other.example.test " + b64([]byte("whatever"))
	// A signature line naming the right log with a key hint that is not this log's key.
	wrongHint := noteMarker + name + " " + b64(append([]byte{0, 0, 0, 0}, 1, 2, 3))
	// A signature line naming the right log, right hint, wrong signature.
	badSig := noteMarker + name + " " + b64(append(append([]byte{}, f.root.logKeyID[:4]...), 9, 9, 9))

	cases := []struct {
		name     string
		envelope string
		root     []byte
		size     int64
		contains string
	}{
		{
			name:     "a checkpoint that is not a signed note",
			envelope: "no blank line here\n",
			contains: "is not a signed note",
		},
		{
			name:     "a note with no signature lines",
			envelope: text + "\n",
			contains: "is not a signed note",
		},
		{
			name:     "a note missing its origin, size or root",
			envelope: name + "\n\n" + goodSig,
			contains: "missing its origin, size, or root",
		},
		{
			name:     "a checkpoint from a different log",
			envelope: "other.example.test - 0\n1\n" + b64(e.rootHash) + "\n\n" + goodSig,
			contains: "comes from other.example.test, not from " + name,
		},
		{
			name:     "a tree size that is not a number",
			envelope: name + " - 0\nmany\n" + b64(e.rootHash) + "\n\n" + goodSig,
			contains: "reading the checkpoint's tree size",
		},
		{
			name:     "a root hash that is not base64",
			envelope: name + " - 0\n1\n!!!\n\n" + goodSig,
			contains: "reading the checkpoint's root hash",
		},
		{
			name:     "a checkpoint about a different tree size",
			envelope: name + " - 0\n99\n" + b64(e.rootHash) + "\n\n" + goodSig,
			contains: "describes a different tree",
		},
		{
			name:     "a checkpoint about a different root",
			envelope: name + " - 0\n1\n" + b64(make([]byte, sha256.Size)) + "\n\n" + goodSig,
			contains: "describes a different tree",
		},
		{
			name:     "a signature line that is not a note signature",
			envelope: text + "\nsigned, the log\n",
			contains: "carries no valid signature",
		},
		{
			name:     "a signature line with no signer",
			envelope: text + "\n" + noteMarker + "justtheone\n",
			contains: "carries no valid signature",
		},
		{
			name:     "a signature line whose blob is not base64",
			envelope: text + "\n" + noteMarker + name + " !!!\n",
			contains: "carries no valid signature",
		},
		{
			name:     "a signature line carrying only a key hint",
			envelope: text + "\n" + noteMarker + name + " " + b64(f.root.logKeyID[:4]) + "\n",
			contains: "carries no valid signature",
		},
		{
			name:     "a signature from another log only",
			envelope: text + "\n" + decoy + "\n",
			contains: "carries no valid signature",
		},
		{
			name:     "a signature selecting a key this binary does not hold",
			envelope: text + "\n" + wrongHint + "\n",
			contains: "carries no valid signature",
		},
		{
			name:     "a signature that does not verify",
			envelope: text + "\n" + badSig + "\n",
			contains: "carries no valid signature",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, size := tc.root, tc.size
			if root == nil {
				root, size = e.rootHash, e.treeSize
			}
			wantFault(t, f.root.verifyCheckpoint(tc.envelope, root, size, name), CodeTransparency, tc.contains)
		})
	}

	// The genuine signature, found past every decoy the note carries in front of it.
	t.Run("a genuine signature behind lines that are not it", func(t *testing.T) {
		env := text + "\n" + decoy + "\n" + wrongHint + "\n" + goodSig
		if err := f.root.verifyCheckpoint(env, e.rootHash, e.treeSize, name); err != nil {
			t.Fatalf("a genuine checkpoint signature was not found: %v", err)
		}
	})
}

func TestSplitNote(t *testing.T) {
	cases := []struct {
		name     string
		envelope string
		wantText string
		wantSigs []string
		wantOK   bool
	}{
		{
			name:     "text and one signature",
			envelope: "a\nb\n\nsig\n",
			wantText: "a\nb\n",
			wantSigs: []string{"sig"},
			wantOK:   true,
		},
		{
			name:     "text and two signatures",
			envelope: "a\n\none\ntwo",
			wantText: "a\n",
			wantSigs: []string{"one", "two"},
			wantOK:   true,
		},
		{name: "no separator", envelope: "a\nb\n"},
		{name: "separator but nothing after it", envelope: "a\n\n"},
		{name: "empty", envelope: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, sigs, ok := splitNote(tc.envelope)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
			if strings.Join(sigs, "|") != strings.Join(tc.wantSigs, "|") {
				t.Errorf("sigs = %q, want %q", sigs, tc.wantSigs)
			}
		})
	}
}

func TestParseNoteSignature(t *testing.T) {
	blob := []byte{1, 2, 3, 4, 5}
	cases := []struct {
		name       string
		line       string
		wantSigner string
		wantBlob   []byte
		wantOK     bool
	}{
		{
			name:       "a well-formed signature line",
			line:       noteMarker + "rekor.sigstore.dev " + b64(blob),
			wantSigner: "rekor.sigstore.dev",
			wantBlob:   blob,
			wantOK:     true,
		},
		{name: "no marker", line: "rekor.sigstore.dev " + b64(blob)},
		{name: "a hyphen instead of the marker", line: "- rekor.sigstore.dev " + b64(blob)},
		{name: "no space after the signer", line: noteMarker + "rekor.sigstore.dev"},
		{name: "a blob that is not base64", line: noteMarker + "rekor.sigstore.dev !!!"},
		{name: "empty", line: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signer, got, ok := parseNoteSignature(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if signer != tc.wantSigner {
				t.Errorf("signer = %q, want %q", signer, tc.wantSigner)
			}
			if string(got) != string(tc.wantBlob) {
				t.Errorf("blob = %x, want %x", got, tc.wantBlob)
			}
		})
	}
}

// The trust anchors are the one thing this package cannot verify at runtime, so a
// broken anchor set has to be refused outright rather than half-loaded: a pool with no
// authority in it, or a log key that is not the log's, would accept anything.
func TestLoadTrustRootRefusesBrokenAnchors(t *testing.T) {
	f := newForge(t)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.caDER})
	logDER, err := x509.MarshalPKIXPublicKey(&f.logKey.PublicKey)
	if err != nil {
		t.Fatalf("marshalling the log key: %v", err)
	}
	logPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: logDER})

	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating an ed25519 key: %v", err)
	}
	edDER, err := x509.MarshalPKIXPublicKey(edPub)
	if err != nil {
		t.Fatalf("marshalling the ed25519 key: %v", err)
	}
	edPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: edDER})

	files := func(chain, key []byte) fstest.MapFS {
		out := fstest.MapFS{}
		if chain != nil {
			out["trust/fulcio.pem"] = &fstest.MapFile{Data: chain}
		}
		if key != nil {
			out["trust/rekor.pub"] = &fstest.MapFile{Data: key}
		}
		return out
	}

	cases := []struct {
		name     string
		fsys     fstest.MapFS
		contains string
	}{
		{
			name:     "no certificate authority file",
			fsys:     files(nil, logPEM),
			contains: "file does not exist",
		},
		{
			name:     "no transparency-log key file",
			fsys:     files(caPEM, nil),
			contains: "file does not exist",
		},
		{
			name:     "an authority chain with nothing in it",
			fsys:     files([]byte("this is not pem"), logPEM),
			contains: "certificate authority chain is empty",
		},
		{
			name:     "an authority chain holding no certificate",
			fsys:     files(logPEM, logPEM),
			contains: "certificate authority chain is empty",
		},
		{
			name:     "an authority chain holding an unparseable certificate",
			fsys:     files(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x30, 0x01}}), logPEM),
			contains: "parsing a certificate authority",
		},
		{
			name:     "an authority chain holding a certificate that is not an authority",
			fsys:     files(f.leafPEM, logPEM),
			contains: "contains a non-CA certificate",
		},
		{
			name:     "a transparency-log key that is not PEM",
			fsys:     files(caPEM, []byte("this is not pem")),
			contains: "log key is not PEM",
		},
		{
			name:     "a transparency-log key that is not a key",
			fsys:     files(caPEM, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{0x30, 0x01}})),
			contains: "parsing the transparency-log key",
		},
		{
			name:     "a transparency-log key that is not an ECDSA key",
			fsys:     files(caPEM, edPEM),
			contains: "log key is not an ECDSA key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadTrustRoot(tc.fsys)
			if err == nil {
				t.Fatal("a broken trust root loaded")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.contains)
			}
		})
	}

	t.Run("a usable trust root", func(t *testing.T) {
		r, err := loadTrustRoot(files(caPEM, logPEM))
		if err != nil {
			t.Fatalf("loadTrustRoot: %v", err)
		}
		// The log id is derived from the key we hold, never read from the bundle: that
		// correspondence is what makes "logged in Rekor" mean the log this binary pinned.
		want := sha256.Sum256(logDER)
		if string(r.logKeyID) != string(want[:]) {
			t.Error("the log id is not the sha256 of the embedded log key")
		}
		if !r.logKey.Equal(&f.logKey.PublicKey) {
			t.Error("the loaded log key is not the key in the anchor file")
		}
	})
}

// The embedded anchors are the ones the binary ships with, and a build whose anchors do
// not load is broken rather than degraded, so first use has to be fatal.
func TestMustTrustRootPanicsOnAnUnusableAnchorSet(t *testing.T) {
	if _, err := newTrustRoot(); err != nil {
		t.Fatalf("the embedded trust root does not load: %v", err)
	}

	original := trustFiles
	t.Cleanup(func() { trustFiles = original })
	trustFiles = embed.FS{}

	defer func() {
		r := recover()
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "the embedded trust root is unusable") {
			t.Errorf("panic = %v, want an unusable-trust-root panic", r)
		}
	}()
	_ = mustTrustRoot()
	t.Error("an unusable trust root was accepted at first use")
}

// The authorities in the embedded chain are sorted by what they are (self-issued or
// not), never by the order they appear in the file. A certificate issued by an
// intermediate has to chain, which it only can if the root landed in the root pool and
// the intermediate in the intermediate pool.
func TestTrustRootSortsAuthoritiesByWhatTheyAre(t *testing.T) {
	f := newForge(t)

	interKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating the intermediate key: %v", err)
	}
	interDER := issueIntermediate(t, f, &interKey.PublicKey)
	interCert, err := x509.ParseCertificate(interDER)
	if err != nil {
		t.Fatalf("parsing the intermediate authority: %v", err)
	}

	ext := fulcioValues(f.policy, forgeRef, forgeCommit)
	leafDER := issueLeaf(t, interCert, interKey, &f.leafKey.PublicKey, ext[oidBuildSignerURI.String()], ext)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.caDER})
	interPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: interDER})
	logDER, err := x509.MarshalPKIXPublicKey(&f.logKey.PublicKey)
	if err != nil {
		t.Fatalf("marshalling the log key: %v", err)
	}

	// The intermediate first, the root second: the wrong order on purpose.
	chain := append(append([]byte{}, interPEM...), caPEM...)
	r, err := loadTrustRoot(fstest.MapFS{
		"trust/fulcio.pem": &fstest.MapFile{Data: chain},
		"trust/rekor.pub":  &fstest.MapFile{Data: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: logDER})},
	})
	if err != nil {
		t.Fatalf("loadTrustRoot: %v", err)
	}
	if _, err := r.verifyChain(leafDER, forgeAt); err != nil {
		t.Fatalf("a certificate issued by the intermediate does not chain: %v", err)
	}
}

// issueIntermediate mints an authority signed by the forge's root authority.
func issueIntermediate(t *testing.T, f *forge, pub *ecdsa.PublicKey) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: "forge intermediate"},
		NotBefore:             forgeAt.Add(-time.Hour),
		NotAfter:              forgeAt.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.caCert, pub, f.caKey)
	if err != nil {
		t.Fatalf("creating the intermediate authority: %v", err)
	}
	return der
}
