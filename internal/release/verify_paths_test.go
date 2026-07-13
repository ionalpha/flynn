package release

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
)

// wantFault asserts the failure a verifier owes its caller: a terminal fault carrying
// the code that says which check refused, and a message naming what was wrong. A test
// that only asserted err != nil would pass even if the bundle were rejected for the
// wrong reason, which is how a check quietly stops doing its job.
func wantFault(t *testing.T, err error, code, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("a bad bundle was accepted; want %s (%s)", code, contains)
	}
	var fe *fault.Error
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a classified fault", err)
	}
	if fe.Class != fault.Terminal {
		t.Errorf("class = %v, want terminal: a refusal is never worth retrying", fe.Class)
	}
	if fe.Code != code {
		t.Errorf("code = %q, want %q (error: %v)", fe.Code, code, err)
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Errorf("error %q does not mention %q", err.Error(), contains)
	}
}

// A verified bundle the test minted itself. Everything downstream of the signature can
// then be tested by changing the signed payload and re-signing, which is the only way
// to reach the checks that only run once a signature has already been believed.
func TestForgedReleaseVerifiesEndToEnd(t *testing.T) {
	f := newForge(t)
	p, err := verifyWith(f.bundleJSON(t, forgeStatement()), f.policy, f.root)
	if err != nil {
		t.Fatalf("the synthetic release does not verify: %v", err)
	}
	if p.Tag != forgeTag {
		t.Errorf("tag = %q, want %q", p.Tag, forgeTag)
	}
	if p.Commit != forgeCommit {
		t.Errorf("commit = %q, want %q", p.Commit, forgeCommit)
	}
	if p.Artifacts[forgeArt] != forgeDigest {
		t.Errorf("artifacts = %v, want %s at %s", p.Artifacts, forgeDigest, forgeArt)
	}
	if p.LogIndex != 4242 || !p.LoggedAt.Equal(forgeAt) {
		t.Errorf("the provenance misplaces the entry: index=%d at=%v", p.LogIndex, p.LoggedAt)
	}
}

// Every check that runs after the signature has been believed, exercised on a bundle
// that is genuine up to that check and re-signed so the signature still holds. These
// are the branches an attacker who has stolen a signing identity reaches.
func TestVerifyRejectsASignedButUnacceptableStatement(t *testing.T) {
	f := newForge(t)

	cases := []struct {
		name     string
		mutate   func(s map[string]any)
		code     string
		contains string
	}{
		{
			name:     "not an in-toto statement",
			mutate:   func(s map[string]any) { s["_type"] = "https://example.test/Statement/v0" },
			code:     CodeStatement,
			contains: "not a SLSA provenance statement",
		},
		{
			name:     "not a SLSA predicate",
			mutate:   func(s map[string]any) { s["predicateType"] = "https://example.test/predicate/v1" },
			code:     CodeStatement,
			contains: "not a SLSA provenance statement",
		},
		{
			name:     "built by an unexpected build type",
			mutate:   func(s map[string]any) { buildDefinition(s)["buildType"] = "https://example.test/buildtype/v1" },
			code:     CodeStatement,
			contains: "unexpected build type",
		},
		{
			name:     "built from another repository",
			mutate:   func(s map[string]any) { workflow(s)["repository"] = "https://github.com/someone/else" },
			code:     CodeStatement,
			contains: "not this project's release workflow",
		},
		{
			name:     "built by another workflow in this repository",
			mutate:   func(s map[string]any) { workflow(s)["path"] = ".github/workflows/ci.yml" },
			code:     CodeStatement,
			contains: "not this project's release workflow",
		},
		{
			name:     "built from a different ref than it was signed on",
			mutate:   func(s map[string]any) { workflow(s)["ref"] = "refs/tags/v0.0.1" },
			code:     CodeStatement,
			contains: "but signed on " + forgeRef,
		},
		{
			name:     "covers no artifacts",
			mutate:   func(s map[string]any) { s["subject"] = []any{} },
			code:     CodeStatement,
			contains: "covers no artifacts",
		},
		{
			name: "covers more artifacts than the ceiling allows",
			mutate: func(s map[string]any) {
				subs := make([]any, 0, maxArtifacts+1)
				for i := range maxArtifacts + 1 {
					subs = append(subs, map[string]any{
						"name":   fmt.Sprintf("flynn_%d.tar.gz", i),
						"digest": map[string]any{"sha256": forgeDigest},
					})
				}
				s["subject"] = subs
			},
			code:     CodeStatement,
			contains: "over the 256 ceiling",
		},
		{
			name: "names an artifact with a path in it",
			mutate: func(s map[string]any) {
				subject(s)["name"] = "../../etc/cron.d/flynn"
			},
			code:     CodeStatement,
			contains: "not a plain file name",
		},
		{
			name: "names an artifact with no name at all",
			mutate: func(s map[string]any) {
				subject(s)["name"] = ""
			},
			code:     CodeStatement,
			contains: "not a plain file name",
		},
		{
			name: "gives an artifact a digest that is not a sha256",
			mutate: func(s map[string]any) {
				subject(s)["digest"] = map[string]any{"sha256": "not-a-digest"}
			},
			code:     CodeStatement,
			contains: "no usable sha256",
		},
		{
			name: "gives one artifact two different digests",
			mutate: func(s map[string]any) {
				s["subject"] = []any{
					map[string]any{"name": forgeArt, "digest": map[string]any{"sha256": forgeDigest}},
					map[string]any{"name": forgeArt, "digest": map[string]any{"sha256": strings.Repeat("2", 64)}},
				}
			},
			code:     CodeStatement,
			contains: "two different digests",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := forgeStatement()
			tc.mutate(s)
			_, err := verifyWith(f.bundleJSON(t, s), f.policy, f.root)
			wantFault(t, err, tc.code, tc.contains)
		})
	}
}

// The same bundle, refused because the policy or the trust root it is held against says
// it is not ours. Each of these is the answer to "what if the attacker's release is
// perfectly genuine, just not this project's".
func TestVerifyRejectsAGenuineReleaseThatIsNotOurs(t *testing.T) {
	f := newForge(t)
	raw := f.bundleJSON(t, forgeStatement())

	t.Run("signed by a workflow in another repository", func(t *testing.T) {
		p := f.policy
		p.repositoryID = "999999"
		_, err := verifyWith(raw, p, f.root)
		wantFault(t, err, CodeIdentity, "unexpected source repository id")
	})

	t.Run("logged in a log whose checkpoint we do not pin", func(t *testing.T) {
		p := f.policy
		p.checkpointName = "some-other-log.example.test"
		_, err := verifyWith(raw, p, f.root)
		wantFault(t, err, CodeTransparency, "not from some-other-log.example.test")
	})

	t.Run("signed by a certificate authority we do not trust", func(t *testing.T) {
		untrusted := &trustRoot{
			roots:         x509.NewCertPool(),
			intermediates: x509.NewCertPool(),
			logKey:        f.root.logKey,
			logKeyID:      f.root.logKeyID,
		}
		_, err := verifyWith(raw, f.policy, untrusted)
		wantFault(t, err, CodeCertificate, "does not chain to a trusted authority")
	})
}

func buildDefinition(s map[string]any) map[string]any {
	return s["predicate"].(map[string]any)["buildDefinition"].(map[string]any)
}

func workflow(s map[string]any) map[string]any {
	return buildDefinition(s)["externalParameters"].(map[string]any)["workflow"].(map[string]any)
}

func subject(s map[string]any) map[string]any {
	return s["subject"].([]any)[0].(map[string]any)
}

// The malformed-bundle table: every field the decoder reads, broken in the way a
// hostile or simply corrupt bundle would break it. The golden bundle is the base, so
// each case is otherwise a real release.
func TestDecodeRejectsMalformedBundles(t *testing.T) {
	cases := []struct {
		name     string
		raw      []byte
		code     string
		contains string
	}{
		{
			name:     "empty",
			raw:      nil,
			code:     CodeBundleDecode,
			contains: "is empty",
		},
		{
			name:     "larger than the ceiling",
			raw:      oversizeBundle(),
			code:     CodeBundleDecode,
			contains: "over the 4194304-byte ceiling",
		},
		{
			name:     "not json",
			raw:      []byte("{"),
			code:     CodeBundleDecode,
			contains: "unexpected end of JSON input",
		},
		{
			name: "certificate is not base64",
			raw: tamper(t, func(m map[string]any) {
				dig(t, m, "verificationMaterial", "certificate")["rawBytes"] = "!!not base64!!"
			}),
			code:     CodeBundleDecode,
			contains: "decoding the signing certificate",
		},
		{
			name: "no certificate",
			raw: tamper(t, func(m map[string]any) {
				dig(t, m, "verificationMaterial", "certificate")["rawBytes"] = ""
			}),
			code:     CodeBundleDecode,
			contains: "carries no signing certificate",
		},
		{
			name: "no payload",
			raw: tamper(t, func(m map[string]any) {
				dig(t, m, "dsseEnvelope")["payload"] = ""
			}),
			code:     CodeBundleDecode,
			contains: "carries no signed payload",
		},
		{
			name: "payload is not base64",
			raw: tamper(t, func(m map[string]any) {
				dig(t, m, "dsseEnvelope")["payload"] = "!!!"
			}),
			code:     CodeBundleDecode,
			contains: "decoding the signed payload",
		},
		{
			name: "no signature at all",
			raw: tamper(t, func(m map[string]any) {
				dig(t, m, "dsseEnvelope")["signatures"] = []any{}
			}),
			code:     CodeBundleDecode,
			contains: "carries 0 signatures",
		},
		{
			name: "two signatures, so it is ambiguous which one was checked",
			raw: tamper(t, func(m map[string]any) {
				env := dig(t, m, "dsseEnvelope")
				sigs := env["signatures"].([]any)
				env["signatures"] = append(sigs, sigs[0])
			}),
			code:     CodeBundleDecode,
			contains: "carries 2 signatures",
		},
		{
			name: "signature is not base64",
			raw: tamper(t, func(m map[string]any) {
				dig(t, m, "dsseEnvelope")["signatures"] = []any{map[string]any{"sig": "!!!"}}
			}),
			code:     CodeBundleDecode,
			contains: "decoding the signature",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeBundle(tc.raw)
			wantFault(t, err, tc.code, tc.contains)
		})
	}
}

func oversizeBundle() []byte {
	raw := make([]byte, maxBundleBytes+1)
	for i := range raw {
		raw[i] = ' '
	}
	return raw
}

// The transparency-log entry is the part of the bundle the verifier cannot skip and
// cannot half-read: every number in it decides how a proof is checked.
func TestSoleTLogEntryRejectsMalformedEntries(t *testing.T) {
	proofField := func(key string, value any) []byte {
		return tamper(t, func(m map[string]any) {
			dig(t, firstTLog(t, m), "inclusionProof")[key] = value
		})
	}
	entryField := func(key string, value any) []byte {
		return tamper(t, func(m map[string]any) {
			firstTLog(t, m)[key] = value
		})
	}

	cases := []struct {
		name     string
		raw      []byte
		code     string
		contains string
	}{
		{
			name: "no entries: nothing says the signature was ever published",
			raw: tamper(t, func(m map[string]any) {
				dig(t, m, "verificationMaterial")["tlogEntries"] = []any{}
			}),
			code:     CodeTransparency,
			contains: "carries 0 transparency-log entries",
		},
		{
			name: "two entries: two answers to whether it was logged",
			raw: tamper(t, func(m map[string]any) {
				vm := dig(t, m, "verificationMaterial")
				entries := vm["tlogEntries"].([]any)
				vm["tlogEntries"] = append(entries, entries[0])
			}),
			code:     CodeTransparency,
			contains: "carries 2 transparency-log entries",
		},
		{
			name: "an entry kind this verifier cannot bind to the envelope",
			raw: tamper(t, func(m map[string]any) {
				dig(t, firstTLog(t, m), "kindVersion")["kind"] = "hashedrekord"
			}),
			code:     CodeTransparency,
			contains: "only knows how to read dsse/0.0.1",
		},
		{
			name: "an entry version this verifier does not know",
			raw: tamper(t, func(m map[string]any) {
				dig(t, firstTLog(t, m), "kindVersion")["version"] = "0.0.2"
			}),
			code:     CodeTransparency,
			contains: "only knows how to read dsse/0.0.1",
		},
		{
			name:     "a log index that is not a number",
			raw:      entryField("logIndex", "seventeen"),
			code:     CodeBundleDecode,
			contains: "reading the log index",
		},
		{
			name:     "an integration time that is not a number",
			raw:      entryField("integratedTime", "yesterday"),
			code:     CodeBundleDecode,
			contains: "reading the integrated time",
		},
		{
			name:     "no integration time, so the certificate window is unanchored",
			raw:      entryField("integratedTime", "0"),
			code:     CodeTransparency,
			contains: "has no integration time",
		},
		{
			name:     "a log key id that is not base64",
			raw:      entryField("logId", map[string]any{"keyId": "!!!"}),
			code:     CodeBundleDecode,
			contains: "decoding the log key id",
		},
		{
			name:     "a logged body that is not base64",
			raw:      entryField("canonicalizedBody", "!!!"),
			code:     CodeBundleDecode,
			contains: "decoding the logged entry body",
		},
		{
			name:     "no signed entry timestamp",
			raw:      entryField("inclusionPromise", map[string]any{"signedEntryTimestamp": ""}),
			code:     CodeBundleDecode,
			contains: "carries no signed entry timestamp",
		},
		{
			name:     "an inclusion proof index that is not a number",
			raw:      proofField("logIndex", "first"),
			code:     CodeBundleDecode,
			contains: "reading the inclusion proof index",
		},
		{
			name:     "an inclusion proof tree size that is not a number",
			raw:      proofField("treeSize", "big"),
			code:     CodeBundleDecode,
			contains: "reading the inclusion proof tree size",
		},
		{
			name:     "an inclusion proof root hash that is not base64",
			raw:      proofField("rootHash", "!!!"),
			code:     CodeBundleDecode,
			contains: "decoding the inclusion proof root hash",
		},
		{
			name:     "an inclusion proof hash that is not base64",
			raw:      proofField("hashes", []any{"!!!"}),
			code:     CodeBundleDecode,
			contains: "decoding the inclusion proof hash 0",
		},
		{
			name:     "no signed checkpoint, so the root hash is unattested",
			raw:      proofField("checkpoint", map[string]any{"envelope": ""}),
			code:     CodeTransparency,
			contains: "carries no signed checkpoint",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := decodeBundle(tc.raw)
			if err != nil {
				wantFault(t, err, tc.code, tc.contains)
				return
			}
			_, err = b.soleTLogEntry()
			wantFault(t, err, tc.code, tc.contains)
		})
	}
}

// The DSSE signature is checked with the digest the signing key's curve calls for. A
// verifier that hard-coded SHA-256 would reject a genuine release the day the key type
// rotates, and one that accepted a key type it cannot reason about would be worse.
func TestDSSESignatureFollowsTheKeysCurve(t *testing.T) {
	const payloadType = "application/vnd.in-toto+json"
	payload := []byte(`{"_type":"https://in-toto.io/Statement/v1"}`)
	pae := dssePAE(payloadType, payload)

	sign := func(t *testing.T, curve elliptic.Curve, digest []byte) (*x509.Certificate, []byte) {
		t.Helper()
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatalf("generating a %v key: %v", curve.Params().Name, err)
		}
		sig, err := ecdsa.SignASN1(rand.Reader, key, digest)
		if err != nil {
			t.Fatalf("signing: %v", err)
		}
		return &x509.Certificate{PublicKey: &key.PublicKey}, sig
	}

	sha384 := sha512.Sum384(pae)
	sha512sum := sha512.Sum512(pae)
	sha256sum := sha256.Sum256(pae)

	t.Run("a P-384 key signs a SHA-384 digest", func(t *testing.T) {
		leaf, sig := sign(t, elliptic.P384(), sha384[:])
		b := &bundle{payload: payload, signature: sig}
		b.DSSEEnvelope.PayloadType = payloadType
		if err := b.verifyDSSESignature(leaf); err != nil {
			t.Fatalf("a genuine P-384 signature was rejected: %v", err)
		}
	})

	t.Run("a P-521 key signs a SHA-512 digest", func(t *testing.T) {
		leaf, sig := sign(t, elliptic.P521(), sha512sum[:])
		b := &bundle{payload: payload, signature: sig}
		b.DSSEEnvelope.PayloadType = payloadType
		if err := b.verifyDSSESignature(leaf); err != nil {
			t.Fatalf("a genuine P-521 signature was rejected: %v", err)
		}
	})

	t.Run("a curve this verifier does not accept", func(t *testing.T) {
		leaf, sig := sign(t, elliptic.P224(), sha256sum[:])
		b := &bundle{payload: payload, signature: sig}
		b.DSSEEnvelope.PayloadType = payloadType
		wantFault(t, b.verifyDSSESignature(leaf), CodeSignature, "unsupported 224-bit curve")
	})

	t.Run("a key that is not ECDSA at all", func(t *testing.T) {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generating an ed25519 key: %v", err)
		}
		b := &bundle{payload: payload, signature: []byte("x")}
		b.DSSEEnvelope.PayloadType = payloadType
		wantFault(t, b.verifyDSSESignature(&x509.Certificate{PublicKey: pub}), CodeSignature, "non-ECDSA key")
	})

	t.Run("a signature over a different payload type", func(t *testing.T) {
		leaf, sig := sign(t, elliptic.P256(), sha256sum[:])
		b := &bundle{payload: payload, signature: sig}
		// The pre-authentication encoding commits to the type, so re-labelling the payload
		// breaks the signature rather than reinterpreting the bytes.
		b.DSSEEnvelope.PayloadType = "application/json"
		wantFault(t, b.verifyDSSESignature(leaf), CodeSignature, "does not verify against the signing certificate")
	})
}

// The certificate identity checks, on certificates a certificate authority would
// genuinely issue. Each case is a build that really happened, signed by a real key,
// that is nonetheless not a flynn release.
func TestCheckIdentityRejectsCertificates(t *testing.T) {
	f := newForge(t)
	p := f.policy
	signerOf := func(ref string) string { return p.repositoryURI + "/" + p.workflowPath + "@" + ref }

	withExt := func(mutate func(ext map[string]string)) map[string]string {
		ext := fulcioValues(p, forgeRef, forgeCommit)
		mutate(ext)
		return ext
	}

	cases := []struct {
		name     string
		leaf     func(t *testing.T) *x509.Certificate
		contains string
	}{
		{
			name: "a certificate naming no identity",
			leaf: func(*testing.T) *x509.Certificate {
				return &x509.Certificate{}
			},
			contains: "names 0 identities",
		},
		{
			name: "a certificate naming two identities",
			leaf: func(t *testing.T) *x509.Certificate {
				a, err := url.Parse(signerOf(forgeRef))
				if err != nil {
					t.Fatal(err)
				}
				return &x509.Certificate{URIs: []*url.URL{a, a}}
			},
			contains: "names 2 identities",
		},
		{
			name: "a certificate carrying no build identity",
			leaf: func(t *testing.T) *x509.Certificate {
				u, err := url.Parse(signerOf(forgeRef))
				if err != nil {
					t.Fatal(err)
				}
				return &x509.Certificate{URIs: []*url.URL{u}, Extensions: []pkix.Extension{
					// An unrelated extension, and a legacy bare-string one: neither is a DER
					// string in the Fulcio arc, so neither is read.
					{Id: asn1.ObjectIdentifier{2, 5, 29, 17}, Value: []byte{0x01}},
					{Id: oidSourceRepoRef, Value: []byte("refs/tags/v9.9.9")},
				}}
			},
			contains: "carries no build identity",
		},
		{
			name: "a certificate whose subject and build signer disagree",
			leaf: func(t *testing.T) *x509.Certificate {
				return f.reissue(t, "https://example.test/somebody-else", fulcioValues(p, forgeRef, forgeCommit))
			},
			contains: "disagree about who signed",
		},
		{
			name: "a signer that is not this project's release workflow",
			leaf: func(t *testing.T) *x509.Certificate {
				// The certificate says it signed on one ref and was issued to a workflow at
				// another: the two have to be the same ref or the signer is not ours.
				ext := withExt(func(ext map[string]string) {
					ext[oidSourceRepoRef.String()] = "refs/heads/main"
				})
				return f.reissue(t, ext[oidBuildSignerURI.String()], ext)
			},
			contains: "not by this project's release workflow",
		},
		{
			name: "a build from a branch rather than a tag",
			leaf: func(t *testing.T) *x509.Certificate {
				ext := fulcioValues(p, "refs/heads/main", forgeCommit)
				return f.reissue(t, ext[oidBuildSignerURI.String()], ext)
			},
			contains: "which is not a version tag",
		},
		{
			name: "a tag that is not a plain version tag",
			leaf: func(t *testing.T) *x509.Certificate {
				ext := fulcioValues(p, "refs/tags/v1/evil", forgeCommit)
				return f.reissue(t, ext[oidBuildSignerURI.String()], ext)
			},
			contains: "is not a plain version tag",
		},
		{
			name: "a certificate naming no source commit",
			leaf: func(t *testing.T) *x509.Certificate {
				ext := fulcioValues(p, forgeRef, "not-a-commit")
				return f.reissue(t, ext[oidBuildSignerURI.String()], ext)
			},
			contains: "names no source commit",
		},
		{
			name: "a certificate from another identity provider",
			leaf: func(t *testing.T) *x509.Certificate {
				ext := withExt(func(ext map[string]string) {
					ext[oidIssuer.String()] = "https://accounts.example.test"
				})
				return f.reissue(t, ext[oidBuildSignerURI.String()], ext)
			},
			contains: "unexpected identity provider",
		},
		{
			name: "a build on a self-hosted runner",
			leaf: func(t *testing.T) *x509.Certificate {
				ext := withExt(func(ext map[string]string) {
					ext[oidRunnerEnv.String()] = "self-hosted"
				})
				return f.reissue(t, ext[oidBuildSignerURI.String()], ext)
			},
			contains: "unexpected runner environment",
		},
		{
			name: "a build triggered by something other than a push",
			leaf: func(t *testing.T) *x509.Certificate {
				ext := withExt(func(ext map[string]string) {
					ext[oidBuildTrigger.String()] = "workflow_dispatch"
				})
				return f.reissue(t, ext[oidBuildSignerURI.String()], ext)
			},
			contains: "unexpected build trigger",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.checkIdentity(tc.leaf(t))
			wantFault(t, err, CodeIdentity, tc.contains)
		})
	}

	t.Run("the identity this project's own release workflow presents", func(t *testing.T) {
		ext := fulcioValues(p, forgeRef, forgeCommit)
		id, err := p.checkIdentity(f.reissue(t, ext[oidBuildSignerURI.String()], ext))
		if err != nil {
			t.Fatalf("a genuine release identity was rejected: %v", err)
		}
		if id.tag != forgeTag || id.ref != forgeRef || id.commit != forgeCommit {
			t.Errorf("identity = %+v", id)
		}
	})
}

// statement() reads the signed payload, and it refuses to read one whose type it does
// not know: a payload the verifier misreads is a payload the attacker gets to choose
// the meaning of.
func TestStatementRefusesPayloadsItCannotRead(t *testing.T) {
	cases := []struct {
		name        string
		payloadType string
		payload     string
		contains    string
	}{
		{
			name:        "a payload of an unknown type",
			payloadType: "application/json",
			payload:     `{}`,
			contains:    "which this verifier does not understand",
		},
		{
			name:        "a payload that is not json",
			payloadType: "application/vnd.in-toto+json",
			payload:     `{"_type":`,
			contains:    "reading the signed provenance",
		},
		{
			name:        "a payload that is not a provenance statement",
			payloadType: "application/vnd.in-toto+json",
			payload:     `{"_type":"https://in-toto.io/Statement/v1","predicateType":"https://example.test/x"}`,
			contains:    "not a SLSA provenance statement",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &bundle{payload: []byte(tc.payload)}
			b.DSSEEnvelope.PayloadType = tc.payloadType
			_, err := b.statement()
			wantFault(t, err, CodeStatement, tc.contains)
		})
	}

	t.Run("the statement the real release carries", func(t *testing.T) {
		b, err := decodeBundle(loadGolden(t))
		if err != nil {
			t.Fatalf("decodeBundle: %v", err)
		}
		s, err := b.statement()
		if err != nil {
			t.Fatalf("statement: %v", err)
		}
		arts, err := s.artifacts()
		if err != nil {
			t.Fatalf("artifacts: %v", err)
		}
		if len(arts) == 0 {
			t.Fatal("the real release covers no artifacts")
		}
		for name, digest := range arts {
			if len(digest) != 64 {
				t.Errorf("artifact %s has digest %q, want a hex sha256", name, digest)
			}
		}
	})
}

// The same artifact listed twice with the same digest is not a contradiction, so it is
// accepted: the map has one answer, and it is the answer both subjects gave.
func TestArtifactsAcceptsARepeatedSubjectWithTheSameDigest(t *testing.T) {
	var s statement
	raw := `{"subject":[
		{"name":"flynn.tar.gz","digest":{"sha256":"` + forgeDigest + `"}},
		{"name":"flynn.tar.gz","digest":{"sha256":"` + strings.ToUpper(forgeDigest) + `"}}]}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	arts, err := s.artifacts()
	if err != nil {
		t.Fatalf("artifacts: %v", err)
	}
	if arts["flynn.tar.gz"] != forgeDigest {
		t.Errorf("artifacts = %v, want the digest normalised to lower case", arts)
	}
}
