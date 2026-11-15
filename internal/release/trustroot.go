package release

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/trustanchor"
)

// The trust anchors are compiled in because a root downloaded at verification time is
// only as trustworthy as the connection it came over, which is the thing defended
// against. Rotating them means shipping a release, an act a user already trusts.
//
// The Rekor key is checkable by anyone: its sha256 is the log id in every Sigstore
// bundle flynn has published, and the trustRoot constructor enforces that at startup.
//
// The anchor files are embedded once in internal/trustanchor, because
// internal/sigstore pins the same Fulcio chain for extension releases and a second
// copy would go stale at Sigstore's first rotation.
var trustFiles fs.FS = trustanchor.Files

// embeddedTrustRoot is the process-wide trust root, built once. A failure here is a
// broken build, so it is fatal at first use instead of an error every caller threads
// through.
var embeddedTrustRoot = mustTrustRoot()

// trustRoot holds the anchors a release signature is checked against: the authority
// that issues signing certificates, and the key of the transparency log that must have
// recorded the signature.
type trustRoot struct {
	roots         *x509.CertPool
	intermediates *x509.CertPool
	logKey        *ecdsa.PublicKey
	logKeyID      []byte
}

func mustTrustRoot() *trustRoot {
	r, err := newTrustRoot()
	if err != nil {
		panic("release: the embedded trust root is unusable: " + err.Error())
	}
	return r
}

func newTrustRoot() (*trustRoot, error) {
	return loadTrustRoot(trustFiles)
}

// loadTrustRoot builds a trust root from a filesystem holding the two anchors. A
// broken anchor set is refused whole, never half-loaded, because this package cannot
// recover from it.
func loadTrustRoot(fsys fs.FS) (*trustRoot, error) {
	chain, err := fs.ReadFile(fsys, "trust/fulcio.pem")
	if err != nil {
		return nil, err
	}
	keyPEM, err := fs.ReadFile(fsys, "trust/rekor.pub")
	if err != nil {
		return nil, err
	}

	r := &trustRoot{roots: x509.NewCertPool(), intermediates: x509.NewCertPool()}

	var certs int
	for rest := chain; len(rest) > 0; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing a certificate authority: %w", err)
		}
		if !c.IsCA {
			return nil, fmt.Errorf("the certificate authority chain contains a non-CA certificate (%s)", c.Subject.CommonName)
		}
		// A root is self-issued and anything else is an intermediate. Sorting by that,
		// instead of file order, keeps the pool correct if upstream reorders the chain.
		if bytes.Equal(c.RawIssuer, c.RawSubject) {
			r.roots.AddCert(c)
		} else {
			r.intermediates.AddCert(c)
		}
		certs++
	}
	if certs == 0 {
		return nil, errors.New("the embedded certificate authority chain is empty")
	}

	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return nil, errors.New("the embedded transparency-log key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the transparency-log key: %w", err)
	}
	ec, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("the embedded transparency-log key is not an ECDSA key")
	}
	r.logKey = ec

	// The log names itself in every bundle by the sha256 of its DER public key. The id
	// is derived from the pinned key, ignoring the bundle's claim, so "logged in Rekor"
	// means the log pinned here.
	sum := sha256.Sum256(blk.Bytes)
	r.logKeyID = sum[:]
	return r, nil
}

// verifyChain checks the signing certificate against the embedded authority as of the
// moment the log recorded the signature, and returns the parsed leaf.
func (r *trustRoot) verifyChain(der []byte, at time.Time) (*x509.Certificate, error) {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeCertificate, fmt.Errorf("parsing the signing certificate: %w", err))
	}
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:         r.roots,
		Intermediates: r.intermediates,
		CurrentTime:   at,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	})
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeCertificate,
			fmt.Errorf("the signing certificate does not chain to a trusted authority as of %s: %w", at.UTC().Format("2006-01-02T15:04:05Z"), err))
	}
	return leaf, nil
}
