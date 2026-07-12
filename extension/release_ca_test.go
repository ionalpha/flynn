package extension

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// testCA is a throwaway certificate authority that mints Fulcio-shaped certificates.
//
// It exists so the resolver's plumbing (download, digest, extract, cache, receipt) can be
// tested against a release the test can construct freely, without vendoring a 3 MB binary
// or being able to forge a real Sigstore signature. The trust *decision* is not tested
// against this CA: that is tested against the real production signature, because a
// signature the test made would only prove the test agrees with itself.
type testCA struct {
	rootPEM  []byte
	rootCert *x509.Certificate
	rootKey  *ecdsa.PrivateKey
}

// Fulcio's claim OIDs, mirrored here because the sigstore package keeps them unexported.
var (
	testOIDIssuer     = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}
	testOIDSourceRepo = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 5}
)

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notBefore := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             notBefore,
		NotAfter:              notBefore.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{
		rootPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		rootCert: cert,
		rootKey:  key,
	}
}

// sign issues a leaf certificate carrying the given identity claims and signs the payload
// with its key, returning what cosign would have written beside the artifact.
func (ca *testCA) sign(t *testing.T, payload []byte, workflow, issuer, sourceRepo string) (certPEM, sig []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(workflow)
	if err != nil {
		t.Fatal(err)
	}
	notBefore := time.Date(2026, 7, 12, 2, 13, 5, 0, time.UTC)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-leaf"},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(10 * time.Minute), // as short-lived as the real thing
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{uri},
		ExtraExtensions: []pkix.Extension{
			{Id: testOIDIssuer, Value: []byte(issuer)},
			{Id: testOIDSourceRepo, Value: []byte(sourceRepo)},
		},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.rootCert, &key.PublicKey, ca.rootKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	digest := sha256.Sum256(payload)
	raw, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, []byte(base64.StdEncoding.EncodeToString(raw))
}
