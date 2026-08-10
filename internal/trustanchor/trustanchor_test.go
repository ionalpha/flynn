package trustanchor_test

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/fs"
	"slices"
	"testing"

	"github.com/ionalpha/flynn/internal/trustanchor"
)

// certsOf decodes every certificate in a PEM bundle, in the order they appear.
func certsOf(t *testing.T, bundle []byte) []*x509.Certificate {
	t.Helper()
	var certs []*x509.Certificate
	for rest := bundle; len(rest) > 0; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatalf("parsing a certificate: %v", err)
		}
		certs = append(certs, c)
	}
	return certs
}

// fingerprints identifies a set of certificates by content, sorted so that two bundles
// holding the same certificates in a different order compare equal.
func fingerprints(certs []*x509.Certificate) []string {
	out := make([]string, 0, len(certs))
	for _, c := range certs {
		out = append(out, fmt.Sprintf("%x %s", sha256.Sum256(c.Raw), c.Subject))
	}
	slices.Sort(out)
	return out
}

// TestTheChainIsTheFulcioCA pins what is pinned. Every other test in the tree can pass
// while trusting a certificate authority nobody chose, because a trust anchor is not
// wrong in any way a functional test can see: it simply decides in someone else's
// favour. So the subjects are asserted here, against the bytes as shipped.
func TestTheChainIsTheFulcioCA(t *testing.T) {
	t.Parallel()
	certs := certsOf(t, trustanchor.Fulcio)
	if len(certs) != 2 {
		t.Fatalf("the chain holds %d certificates, want the Fulcio root and its intermediate", len(certs))
	}

	var root, intermediate *x509.Certificate
	for _, c := range certs {
		if bytes.Equal(c.RawIssuer, c.RawSubject) {
			root = c
		} else {
			intermediate = c
		}
	}
	if root == nil || intermediate == nil {
		t.Fatalf("the chain is not a root plus an intermediate: %v", fingerprints(certs))
	}
	if got, want := root.Subject.String(), "CN=sigstore,O=sigstore.dev"; got != want {
		t.Errorf("root is %q, want %q", got, want)
	}
	if got, want := intermediate.Subject.String(), "CN=sigstore-intermediate,O=sigstore.dev"; got != want {
		t.Errorf("intermediate is %q, want %q", got, want)
	}
	// An intermediate the root did not issue would leave the chain unbuildable, and the
	// failure would surface as "this release is not signed" long after the file changed.
	if err := intermediate.CheckSignatureFrom(root); err != nil {
		t.Errorf("the root did not issue the intermediate: %v", err)
	}
	for _, c := range certs {
		if !c.IsCA {
			t.Errorf("%s is not a CA certificate, so it cannot anchor anything", c.Subject)
		}
	}
}

// TestBothFormsHoldTheSameChain covers the one way this package can be internally
// inconsistent: Fulcio and Files are separate go:embed directives over the same path, and
// a mistyped path in either would compile and then quietly hand a caller nothing.
func TestBothFormsHoldTheSameChain(t *testing.T) {
	t.Parallel()
	fromFS, err := fs.ReadFile(trustanchor.Files, "trust/fulcio.pem")
	if err != nil {
		t.Fatalf("reading trust/fulcio.pem from Files: %v", err)
	}
	if !bytes.Equal(fromFS, trustanchor.Fulcio) {
		t.Error("Fulcio and Files disagree about the chain")
	}
	if _, err := fs.ReadFile(trustanchor.Files, "trust/rekor.pub"); err != nil {
		t.Fatalf("reading trust/rekor.pub from Files: %v", err)
	}
}

// TestRekorKeyIsTheLogWePinned checks the transparency-log key by the identity the log
// states in every bundle, which is the sha256 of its DER encoding. Anyone can reproduce
// this number from a published Sigstore bundle, so a substituted key is caught here
// rather than by a release that mysteriously stops verifying.
func TestRekorKeyIsTheLogWePinned(t *testing.T) {
	t.Parallel()
	keyPEM, err := fs.ReadFile(trustanchor.Files, "trust/rekor.pub")
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		t.Fatal("the transparency-log key is not PEM")
	}
	const logID = "c0d23d6ad406973f9559f3ba2d1ca01f84147d8ffc5b8445c224f98b9591801d"
	if got := fmt.Sprintf("%x", sha256.Sum256(blk.Bytes)); got != logID {
		t.Errorf("log id is %s, want %s", got, logID)
	}
}
