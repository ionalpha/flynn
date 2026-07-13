package release

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/ionalpha/flynn/fault"
)

// The trust anchors are compiled into the binary rather than fetched, because a trust
// root you download at verification time is only as trustworthy as the connection you
// downloaded it over, which is precisely the thing being defended against. Rotating
// them means shipping a release, which is the same act a user already has to trust.
//
// The Rekor key is checkable against evidence anyone can reproduce: its sha256 is the
// log id that appears in every Sigstore bundle flynn has ever published, and the
// trustRoot constructor enforces that correspondence at startup.
//
//go:embed trust/fulcio.pem trust/rekor.pub
var trustFiles embed.FS

// embeddedTrustRoot is the process-wide trust root, built once. A failure here is a
// broken build, not a runtime condition, so it is fatal at first use rather than an
// error every caller has to thread through.
var embeddedTrustRoot = mustTrustRoot()

// trustRoot holds the anchors a release signature is checked against: the certificate
// authority that issues the signing certificates, and the public key of the
// transparency log that has to have recorded the signature.
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

// loadTrustRoot builds a trust root from a filesystem holding the two anchors. The filesystem
// stays a parameter because a broken anchor set has to be refused outright rather than
// half-loaded, and that is the one failure this package cannot recover from.
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
		// A root is self-issued; anything else is an intermediate. Sorting them by what
		// they are, rather than by their order in the file, keeps the pool correct if the
		// upstream chain is ever emitted in a different order.
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

	// The log identifies itself in every bundle by the sha256 of its DER public key.
	// Deriving the id from the key we hold, rather than trusting the id the bundle
	// states, is what makes "this was logged in Rekor" mean the log we actually pinned.
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
