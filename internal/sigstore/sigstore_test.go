package sigstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/fault"
)

// codeOf reports the stable fault code of an error, so a test asserts *why* verification
// was refused rather than merely that it was. A check that fails for an incidental reason
// (a typo in a fixture, say) would otherwise look like the security property holding.
func codeOf(err error) string {
	var fe *fault.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

// The testdata is not synthetic: it is the checksums.txt, signature and certificate from
// the real token/v0.1.0 release of ionalpha/flynn-extensions. Verifying against a fixture
// this package generated itself would only prove it agrees with itself; verifying the
// artifact a user actually downloads proves it agrees with Sigstore.
const (
	realWorkflow   = "https://github.com/ionalpha/go-ci/.github/workflows/monorepo-release.yml@refs/heads/main"
	realIssuer     = "https://token.actions.githubusercontent.com"
	realSourceRepo = "ionalpha/flynn-extensions"
)

func realIdentity() Identity {
	return Identity{Workflow: realWorkflow, Issuer: realIssuer, SourceRepo: realSourceRepo}
}

func load(t *testing.T) (payload, sig, cert []byte) {
	t.Helper()
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return read("checksums.txt"), read("checksums.txt.sig"), read("checksums.txt.pem")
}

// TestVerifyRealRelease is the test that matters: the artifacts a user downloads today
// verify against the pinned identity.
func TestVerifyRealRelease(t *testing.T) {
	t.Parallel()
	payload, sig, cert := load(t)
	if err := Verify(payload, sig, cert, realIdentity()); err != nil {
		t.Fatalf("the real token/v0.1.0 release does not verify: %v", err)
	}
}

// TestVerifyRejects covers every way a signature can fail to prove what it claims. Each
// case must be refused, and refused as Forbidden rather than as some incidental error, so
// a caller cannot mistake "could not check" for "checked and fine".
func TestVerifyRejects(t *testing.T) {
	t.Parallel()
	payload, sig, cert := load(t)

	tests := []struct {
		name    string
		payload []byte
		sig     []byte
		cert    []byte
		id      Identity
		code    string
	}{
		{
			// The whole point: a tampered artifact list no longer matches the signature,
			// so a swapped binary cannot ride in under a genuine release's signature.
			name:    "tampered payload",
			payload: append([]byte("evil\n"), payload...),
			sig:     sig, cert: cert, id: realIdentity(),
			code: "sigstore_bad_signature",
		},
		{
			name:    "a single flipped byte in the payload",
			payload: flip(payload),
			sig:     sig, cert: cert, id: realIdentity(),
			code: "sigstore_bad_signature",
		},
		{
			name:    "signature from a different payload",
			payload: payload,
			sig:     []byte("MEUCIQDDoSj3aQmS0KCM0T4mRLIYSKDJz3rEJlbXqzWFbNRkNQIgLcXW7BQ4pXfLkzO6dGNfJ4Zc0xIYbmvhEXpKvvPQ9Fo="),
			cert:    cert, id: realIdentity(),
			code: "sigstore_bad_signature",
		},
		{
			// A real, correctly-signed release from someone else's workflow. This is the
			// case a naive "is the signature valid" check waves through.
			name:    "valid signature, untrusted workflow",
			payload: payload, sig: sig, cert: cert,
			id:   Identity{Workflow: "https://github.com/attacker/evil/.github/workflows/release.yml@refs/heads/main", Issuer: realIssuer, SourceRepo: realSourceRepo},
			code: "sigstore_wrong_workflow",
		},
		{
			// The identity string alone is not enough: it must have been vouched for by
			// the issuer we trust, or anyone running their own OIDC provider could mint it.
			name:    "right workflow, untrusted issuer",
			payload: payload, sig: sig, cert: cert,
			id:   Identity{Workflow: realWorkflow, Issuer: "https://evil.example/oidc", SourceRepo: realSourceRepo},
			code: "sigstore_wrong_issuer",
		},
		{
			// The workflow is reusable and shared across repos, so the signing identity
			// does not by itself say which repo was released.
			name:    "right workflow, wrong source repository",
			payload: payload, sig: sig, cert: cert,
			id:   Identity{Workflow: realWorkflow, Issuer: realIssuer, SourceRepo: "attacker/flynn-extensions"},
			code: "sigstore_wrong_source_repo",
		},
		{
			// Failing to pin must never mean "accept anything".
			name:    "unpinned identity",
			payload: payload, sig: sig, cert: cert,
			id:   Identity{},
			code: "sigstore_identity_unpinned",
		},
		{
			name:    "partially pinned identity",
			payload: payload, sig: sig, cert: cert,
			id:   Identity{Workflow: realWorkflow},
			code: "sigstore_identity_unpinned",
		},
		{
			name:    "certificate is not a certificate",
			payload: payload, sig: sig,
			cert: []byte("-----BEGIN CERTIFICATE-----\nbm90IGEgY2VydA==\n-----END CERTIFICATE-----\n"),
			id:   realIdentity(),
			code: "sigstore_cert_parse",
		},
		{
			name:    "no certificate at all",
			payload: payload, sig: sig, cert: []byte("hello"),
			id:   realIdentity(),
			code: "sigstore_cert_not_pem",
		},
		{
			name:    "signature is not base64",
			payload: payload, sig: []byte("!!!not base64!!!"), cert: cert,
			id:   realIdentity(),
			code: "sigstore_sig_not_base64",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Verify(tc.payload, tc.sig, tc.cert, tc.id)
			if err == nil {
				t.Fatalf("verification succeeded, want it refused with %s", tc.code)
			}
			if got := codeOf(err); got != tc.code {
				t.Errorf("refused with code %q, want %q (error: %v)", got, tc.code, err)
			}
		})
	}
}

// TestSelfSignedCertificateIsRefused checks the chain pin specifically. An attacker who
// mints their own certificate can put any identity they like in it, so the identity checks
// are worthless unless the certificate is known to come from Fulcio.
func TestSelfSignedCertificateIsRefused(t *testing.T) {
	t.Parallel()
	payload, _, _ := load(t)

	// A certificate carrying exactly the pinned identity, signed by nobody in particular.
	certPEM, sig := forgeSignedPayload(t, payload, realWorkflow, realIssuer, realSourceRepo)

	err := Verify(payload, sig, certPEM, realIdentity())
	if err == nil {
		t.Fatal("a self-signed certificate claiming our identity was accepted; the chain is not being checked")
	}
	if got := codeOf(err); got != "sigstore_chain_untrusted" {
		t.Errorf("refused with %q, want sigstore_chain_untrusted (error: %v)", got, err)
	}
}

// TestEmbeddedRootsAreTheRealOnes guards the trust anchor itself. If these bytes are ever
// replaced, every other test in this file would still pass while trusting a different CA.
func TestEmbeddedRootsAreTheRealOnes(t *testing.T) {
	t.Parallel()
	if len(fulcioRoots) == 0 {
		t.Fatal("no Fulcio roots are embedded, so nothing is pinned")
	}
	for _, want := range []string{"CN=sigstore", "CN=sigstore-intermediate"} {
		if !strings.Contains(subjectsOf(t, fulcioRoots), want) {
			t.Errorf("embedded roots do not contain %q", want)
		}
	}
}
