package sigstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"
)

// forgeSignedPayload mints a certificate carrying whatever identity the caller asks for,
// signed by a CA of its own invention, and a real signature over the payload under that
// certificate's key.
//
// This is the attacker who cannot compromise our workflow but can certainly generate keys.
// Every identity claim in the result is exactly the one we pin, and the signature over the
// payload genuinely verifies under the certificate. The only thing wrong with it is that
// Fulcio never issued it, which is the whole reason the chain is pinned.
func forgeSignedPayload(t *testing.T, payload []byte, workflow, issuer, sourceRepo string) (certPEM, sig []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(workflow)
	if err != nil {
		t.Fatal(err)
	}
	// A fixed window, so the test does not depend on the clock.
	notBefore := time.Date(2026, 7, 12, 2, 13, 5, 0, time.UTC)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "forged"},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(10 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{uri},
		ExtraExtensions: []pkix.Extension{
			{Id: oidIssuer, Value: []byte(issuer)},
			{Id: oidSourceRepo, Value: []byte(sourceRepo)},
		},
		BasicConstraintsValid: true,
		IsCA:                  true, // self-issued, so it must be able to sign itself
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	digest := sha256.Sum256(payload)
	raw, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig = []byte(base64.StdEncoding.EncodeToString(raw))

	// Sanity: the forgery really is internally consistent, so the test below is proving
	// the chain check works and not that the forgery was botched.
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkIdentity(cert, Identity{Workflow: workflow, Issuer: issuer, SourceRepo: sourceRepo}); err != nil {
		t.Fatalf("forged certificate does not carry the pinned identity, so the test proves nothing: %v", err)
	}
	if err := checkSignature(cert, payload, sig); err != nil {
		t.Fatalf("forged signature does not verify under its own certificate, so the test proves nothing: %v", err)
	}
	return certPEM, sig
}

// flip returns the payload with one byte changed.
func flip(b []byte) []byte {
	out := append([]byte(nil), b...)
	out[len(out)/2] ^= 0x01
	return out
}

// subjectsOf renders the subjects of a PEM bundle, for asserting what is pinned.
func subjectsOf(t *testing.T, bundle []byte) string {
	t.Helper()
	var subjects []string
	rest := bundle
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		subjects = append(subjects, c.Subject.String())
	}
	return strings.Join(subjects, "\n")
}
