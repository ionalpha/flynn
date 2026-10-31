package release

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
)

// goldenBundle is the Sigstore bundle GitHub actually published for flynn v0.1.3-rc.1.
// The verifier is tested against the real artifact, not against one the test made up,
// because a verifier that only ever sees bundles built by its own encoder proves
// nothing about the bundles it will meet in the field.
const goldenBundle = "testdata/v0.1.3-rc.1.intoto.json"

func loadGolden(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(goldenBundle)
	if err != nil {
		t.Fatalf("reading the golden bundle: %v", err)
	}
	return raw
}

func TestVerifyAcceptsTheRealRelease(t *testing.T) {
	p, err := Verify(loadGolden(t))
	if err != nil {
		t.Fatalf("the real published release does not verify: %v", err)
	}

	if p.Tag != "v0.1.3-rc.1" {
		t.Errorf("tag = %q, want v0.1.3-rc.1", p.Tag)
	}
	if p.Commit != "22dba388133eab33e7c9fc5b8fe8b50029e1d9d1" {
		t.Errorf("commit = %q", p.Commit)
	}
	if !strings.HasSuffix(p.SignerIdentity, ".github/workflows/release.yml@refs/tags/v0.1.3-rc.1") {
		t.Errorf("signer = %q", p.SignerIdentity)
	}
	if p.LogIndex == 0 || p.LoggedAt.IsZero() {
		t.Errorf("the provenance does not place the signature in the log: index=%d at=%v", p.LogIndex, p.LoggedAt)
	}

	// The digest a caller would pin an upgrade to.
	got, err := p.Digest("flynn_linux_amd64.tar.gz")
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if len(got) != 64 {
		t.Errorf("digest = %q, want a hex sha256", got)
	}
	if _, err := p.Digest("flynn_linux_amd64.tar.gz.evil"); err == nil {
		t.Error("Digest invented a digest for an artifact the release never covered")
	}
}

// The certificate that signed the release expired ten minutes after it was issued.
// Verification therefore has to happen as of the moment the log recorded the entry,
// and a verifier that checked "now" would reject every genuine release. This test
// fails the day someone replaces the integration time with time.Now().
func TestVerifyChecksTheCertificateAtLogTime(t *testing.T) {
	p, err := Verify(loadGolden(t))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	b, err := decodeBundle(loadGolden(t))
	if err != nil {
		t.Fatalf("decodeBundle: %v", err)
	}
	leaf, err := embeddedTrustRoot.verifyChain(b.leafCertificate, p.LoggedAt)
	if err != nil {
		t.Fatalf("verifyChain at log time: %v", err)
	}
	if life := leaf.NotAfter.Sub(leaf.NotBefore); life > 15*60*1e9 {
		t.Errorf("signing certificate lives %v; a Fulcio certificate is expected to be short-lived", life)
	}
	if _, err := embeddedTrustRoot.verifyChain(b.leafCertificate, leaf.NotAfter.Add(1)); err == nil {
		t.Error("the certificate verified after it expired")
	}
	if _, err := embeddedTrustRoot.verifyChain(b.leafCertificate, leaf.NotBefore.Add(-1)); err == nil {
		t.Error("the certificate verified before it was issued")
	}
}

// The embedded log key must be the key that signed our own published release: its
// sha256 is the log id the bundle carries. This is the one trust anchor a reader can
// check for themselves without leaving the repository.
func TestEmbeddedLogKeyMatchesTheLogIDInOurOwnRelease(t *testing.T) {
	b, err := decodeBundle(loadGolden(t))
	if err != nil {
		t.Fatalf("decodeBundle: %v", err)
	}
	e, err := b.soleTLogEntry()
	if err != nil {
		t.Fatalf("soleTLogEntry: %v", err)
	}
	if string(e.keyID) != string(embeddedTrustRoot.logKeyID) {
		t.Fatal("the embedded transparency-log key is not the key that signed our own published release")
	}
}

// tamper rewrites one field of the golden bundle and returns the result. Every case
// below is a forgery a real attacker would attempt, and every one of them must fail.
func tamper(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(loadGolden(t), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mutate(m)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func dig(t *testing.T, m map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := m
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("path %v is not an object at %q", path, p)
		}
		cur = next
	}
	return cur
}

func firstTLog(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	entries, ok := dig(t, m, "verificationMaterial")["tlogEntries"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatal("no tlog entries")
	}
	e, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatal("tlog entry is not an object")
	}
	return e
}

func TestVerifyRejectsForgeries(t *testing.T) {
	// A digest swapped in the signed payload: the attacker wants us to install their
	// binary under our name. The payload is covered by the signature, so this must fail
	// at the signature, not at some later sanity check.
	swappedPayload := tamper(t, func(m map[string]any) {
		dig(t, m, "dsseEnvelope")["payload"] = "eyJfdHlwZSI6Imh0dHBzOi8vaW4tdG90by5pby9TdGF0ZW1lbnQvdjEifQ=="
	})

	// The signature replaced with a well-formed but wrong one.
	brokenSig := tamper(t, func(m map[string]any) {
		dig(t, m, "dsseEnvelope")["signatures"] = []any{map[string]any{
			"sig": "MEUCIQD3ZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQIgZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQZQY=",
		}}
	})

	// A signature that was never published in the transparency log: the inclusion proof
	// is stripped. A verifier that treats the log as optional accepts a compromise that
	// nobody can ever detect, so the missing proof has to be fatal.
	noProof := tamper(t, func(m map[string]any) {
		delete(firstTLog(t, m), "inclusionProof")
	})

	// The inclusion proof kept but its root hash rewritten, so the proof reconstructs a
	// tree the log never signed a checkpoint for.
	forgedRoot := tamper(t, func(m map[string]any) {
		dig(t, firstTLog(t, m), "inclusionProof")["rootHash"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	})

	// The entry pointed at a different log, whose key we do not pin.
	otherLog := tamper(t, func(m map[string]any) {
		dig(t, firstTLog(t, m), "logId")["keyId"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	})

	// A truncated certificate: the identity cannot be established at all.
	noCert := tamper(t, func(m map[string]any) {
		dig(t, m, "verificationMaterial", "certificate")["rawBytes"] = "QUJD"
	})

	// A bundle claiming to be some other format, which this verifier must not try to
	// interpret with rules that were written for a different one.
	wrongMediaType := tamper(t, func(m map[string]any) {
		m["mediaType"] = "application/vnd.example.bundle+json"
	})

	cases := map[string][]byte{
		"payload swapped under the signature": swappedPayload,
		"signature replaced":                  brokenSig,
		"never published in the log":          noProof,
		"inclusion proof root forged":         forgedRoot,
		"logged in an untrusted log":          otherLog,
		"certificate unparseable":             noCert,
		"unknown bundle format":               wrongMediaType,
		"empty bundle":                        {},
		"not json":                            []byte("{{{"),
	}

	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(raw); err == nil {
				t.Fatal("a forged release verified")
			} else if fault.Classify(err) != fault.Terminal {
				// A forgery is never worth retrying. Classifying it as transient would put a
				// retry loop between an attacker and the install path.
				t.Errorf("a forged release failed as %v, want terminal", fault.Classify(err))
			}
		})
	}
}

// The identity checks are what stop a genuine Sigstore signature, from a genuine
// GitHub workflow that simply is not ours, from installing itself as flynn. Each of
// these is a real certificate the public good instance would happily issue.
func TestIdentityPolicyRejectsForeignSigners(t *testing.T) {
	raw := loadGolden(t)
	b, err := decodeBundle(raw)
	if err != nil {
		t.Fatalf("decodeBundle: %v", err)
	}
	e, err := b.soleTLogEntry()
	if err != nil {
		t.Fatalf("soleTLogEntry: %v", err)
	}
	leaf, err := embeddedTrustRoot.verifyChain(b.leafCertificate, e.integratedTime)
	if err != nil {
		t.Fatalf("verifyChain: %v", err)
	}

	if _, err := defaultPolicy().checkIdentity(leaf); err != nil {
		t.Fatalf("the real release's identity was rejected: %v", err)
	}

	cases := map[string]func(p *policy){
		"a fork of the same repository, same workflow path": func(p *policy) { p.repositoryID = "999999" },
		"a repository name reclaimed after a rename":        func(p *policy) { p.repositoryURI = "https://github.com/ionalpha/flynn-old" },
		"a different owner":                                 func(p *policy) { p.ownerID = "999999" },
		"a different workflow in this repository":           func(p *policy) { p.workflowPath = ".github/workflows/ci.yml" },
		"a different identity provider":                     func(p *policy) { p.oidcIssuer = "https://accounts.google.com" },
		"a self-hosted runner":                              func(p *policy) { p.runnerEnv = "self-hosted" },
		"a branch build rather than a tag":                  func(p *policy) { p.tagRefPrefix = "refs/heads/" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := defaultPolicy()
			mutate(&p)
			if _, err := p.checkIdentity(leaf); err == nil {
				t.Fatal("an identity that is not this project's release workflow was accepted")
			}
		})
	}
}

// The provenance and the certificate have to agree. A statement built from one ref and
// signed on another is two builds stapled together.
func TestStatementMustAgreeWithTheCertificate(t *testing.T) {
	b, err := decodeBundle(loadGolden(t))
	if err != nil {
		t.Fatalf("decodeBundle: %v", err)
	}
	s, err := b.statement()
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	if err := defaultPolicy().checkStatement(s, identity{ref: "refs/tags/v0.1.3-rc.1"}); err != nil {
		t.Fatalf("the real statement was rejected: %v", err)
	}
	if err := defaultPolicy().checkStatement(s, identity{ref: "refs/tags/v9.9.9"}); err == nil {
		t.Fatal("a statement built from a different ref than it was signed on was accepted")
	}
}
