// Package sigstore verifies keyless Sigstore (cosign) signatures in process.
//
// A released extension is code flynn is about to execute, so who produced it is
// checked before it runs, by the binary that will run it. Shelling out to cosign would
// make verification depend on a tool most users lack, and a missing tool fails by
// skipping the check.
//
// Keyless signing has no long-lived key to pin. The release workflow exchanges its
// OIDC token for a short-lived Fulcio certificate that names the workflow, so the pin
// is an identity: this workflow, from this repository, attested by this issuer, under
// a certificate chaining to Sigstore's root. Forging it means controlling the release
// workflow.
//
// What this verifies:
//
//   - the certificate chains to the pinned Fulcio root
//   - it was issued for the pinned workflow identity (its SAN URI)
//   - the OIDC issuer is the pinned one, so an identity string minted by some other issuer
//     cannot satisfy the check
//   - the source repository recorded in the certificate is the pinned one
//   - the signature verifies over the payload under the certificate's key
//
// It does not verify Rekor inclusion or the embedded SCT. Those defend against a
// compromised or coerced Fulcio issuing a certificate nobody can see, and are worth
// adding. They do not guard this check today: a certificate that satisfies the
// identity, issuer and repository pins requires our workflow's OIDC token, and the
// holder of that token can publish a genuinely logged release anyway.
package sigstore

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/trustanchor"
)

// fulcioRoots is Sigstore's public-good Fulcio CA chain (root plus intermediate),
// embedded so verification needs no network and no on-disk trust store a local
// attacker could edit; a root fetched at run time is one an attacker can substitute. It
// comes from internal/trustanchor because internal/release pins the same chain, and two
// copies would drift at the first rotation.
var fulcioRoots = trustanchor.Fulcio

// Fulcio records the OIDC token's claims as certificate extensions under Sigstore's
// private enterprise OID arc. These are the ones this package pins.
var (
	// oidIssuer is the OIDC issuer that vouched for the identity, in the v1 plain-string
	// form. The v2 form (arc 1.8) carries the same value DER-encoded; v1 is still issued
	// and unambiguous to read.
	oidIssuer = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}

	// oidSourceRepo is the repository the workflow ran for, e.g.
	// "ionalpha/flynn-extensions". The identity names the signing workflow, which for a
	// reusable workflow lives in another repository, so without this claim a release
	// built by the right workflow for the wrong repository would pass.
	oidSourceRepo = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 5}
)

// Identity is what a signature must prove about its origin before the artifact it
// covers is trusted. Every field is required, so a zero Identity verifies nothing and a
// caller that forgets to pin accepts nothing.
type Identity struct {
	// Workflow is the certificate's SAN URI: the workflow that requested the signature.
	// For a release built by a reusable workflow this is the reusable workflow's ref,
	// because that is what Sigstore binds the signature to.
	Workflow string

	// Issuer is the OIDC issuer that authenticated the workflow.
	Issuer string

	// SourceRepo is the "owner/name" the workflow ran for.
	SourceRepo string
}

// Verify checks a detached keyless signature over payload and reports whether it was made
// by the pinned identity. It returns nil only when every pin holds.
//
// sig is the base64 signature and certPEM the certificate, exactly as cosign writes them
// beside the artifact (`--output-signature`, `--output-certificate`).
func Verify(payload, sig, certPEM []byte, want Identity) error {
	return Verifier{}.Verify(payload, sig, certPEM, want)
}

// Verifier is Verify with the trust anchor made explicit. The zero value uses the
// embedded Fulcio roots; a caller supplies its own only to verify against a different
// Sigstore instance.
type Verifier struct {
	// Roots is a PEM bundle to trust instead of the embedded Fulcio chain.
	Roots []byte
}

// Verify checks a detached keyless signature against the verifier's trust anchor.
func (v Verifier) Verify(payload, sig, certPEM []byte, want Identity) error {
	if want.Workflow == "" || want.Issuer == "" || want.SourceRepo == "" {
		return fault.New(fault.Terminal, "sigstore_identity_unpinned",
			"sigstore: refusing to verify against an incomplete identity; workflow, issuer and source repo must all be pinned")
	}

	cert, err := parseCertificate(certPEM)
	if err != nil {
		return err
	}
	if err := checkChain(cert, v.roots()); err != nil {
		return err
	}
	if err := checkIdentity(cert, want); err != nil {
		return err
	}
	return checkSignature(cert, payload, sig)
}

// parseCertificate decodes the certificate cosign emitted. cosign base64-encodes the
// PEM, so both forms are accepted; anything else is refused.
func parseCertificate(certPEM []byte) (*x509.Certificate, error) {
	raw := certPEM
	if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw))); err == nil {
		raw = dec
	}
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fault.New(fault.Terminal, "sigstore_cert_not_pem",
			"sigstore: signing certificate is not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "sigstore_cert_parse", err)
	}
	return cert, nil
}

// checkChain verifies the certificate against the trust roots as of the moment it was
// issued. A Fulcio certificate lives ten minutes, so it has always expired by the time
// anyone installs the release it signed, and verifying as of now would reject every
// genuine release. The validity window is signed by the CA, so an attacker cannot
// widen it, and the binding checked (this CA issued this key to this identity) does
// not depend on time.
func checkChain(cert *x509.Certificate, bundle []byte) error {
	roots := x509.NewCertPool()
	intermediates := x509.NewCertPool()

	rest := bundle
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return fault.Wrap(fault.Terminal, "sigstore_root_parse", err)
		}
		// The chain ships as root plus intermediate; the self-issued certificate is the
		// root.
		if c.Subject.String() == c.Issuer.String() {
			roots.AddCert(c)
		} else {
			intermediates.AddCert(c)
		}
	}

	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   cert.NotBefore,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}); err != nil {
		return fault.Wrap(fault.Forbidden, "sigstore_chain_untrusted", err)
	}
	return nil
}

// roots is the trust anchor in use: Roots if set, else the embedded Fulcio chain.
// Production never sets Roots, so production always pins Fulcio.
func (v Verifier) roots() []byte {
	if len(v.Roots) > 0 {
		return v.Roots
	}
	return fulcioRoots
}

func checkIdentity(cert *x509.Certificate, want Identity) error {
	if len(cert.URIs) == 0 {
		return fault.New(fault.Forbidden, "sigstore_no_identity",
			"sigstore: certificate carries no SAN URI, so it names no signing workflow")
	}
	if got := cert.URIs[0].String(); got != want.Workflow {
		return fault.New(fault.Forbidden, "sigstore_wrong_workflow",
			"sigstore: signed by "+got+", but only "+want.Workflow+" is trusted to sign this artifact")
	}

	issuer, err := extensionString(cert, oidIssuer)
	if err != nil {
		return err
	}
	if issuer != want.Issuer {
		// Without this, an identity string that looks like ours, vouched for by another
		// OIDC issuer, would satisfy the workflow pin above.
		return fault.New(fault.Forbidden, "sigstore_wrong_issuer",
			"sigstore: identity was issued by "+issuer+", not the trusted issuer "+want.Issuer)
	}

	repo, err := extensionString(cert, oidSourceRepo)
	if err != nil {
		return err
	}
	if repo != want.SourceRepo {
		return fault.New(fault.Forbidden, "sigstore_wrong_source_repo",
			"sigstore: artifact was built for "+repo+", not "+want.SourceRepo)
	}
	return nil
}

// extensionString reads a Fulcio claim extension as a string. The v1 claims are raw
// UTF-8 bytes with no DER wrapping, so they are read directly.
func extensionString(cert *x509.Certificate, oid asn1.ObjectIdentifier) (string, error) {
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(oid) {
			return string(ext.Value), nil
		}
	}
	return "", fault.New(fault.Forbidden, "sigstore_missing_claim",
		"sigstore: certificate is missing the "+oid.String()+" claim, so its origin cannot be established")
}

// checkSignature verifies the detached signature over the payload under the certificate's
// public key. cosign signs the SHA-256 of the payload with ECDSA.
func checkSignature(cert *x509.Certificate, payload, sig []byte) error {
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fault.New(fault.Forbidden, "sigstore_key_type",
			"sigstore: signing certificate does not carry an ECDSA key")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return fault.Wrap(fault.Terminal, "sigstore_sig_not_base64", err)
	}
	digest := sha256.Sum256(payload)
	if !ecdsa.VerifyASN1(pub, digest[:], raw) {
		return fault.New(fault.Forbidden, "sigstore_bad_signature",
			"sigstore: signature does not verify over this payload; the artifact or the signature has been altered")
	}
	return nil
}
