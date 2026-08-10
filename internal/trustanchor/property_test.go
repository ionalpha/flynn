package trustanchor_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/internal/trustanchor"
)

// emit re-encodes certificates as a PEM bundle, optionally with text between the blocks.
// PEM readers skip anything outside a BEGIN/END pair, which is how a bundle can carry
// human-readable headers, so a bundle is not a byte sequence but a set of blocks.
func emit(certs []*x509.Certificate, between string) []byte {
	var out []byte
	for _, c := range certs {
		out = append(out, []byte(between)...)
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return append(out, []byte(between)...)
}

// TestPropertyTheAnchorIsItsContent states what may and may not be done to the anchor
// file, which is the question anyone rotating it has to answer.
//
// The two packages that read this chain both sort it by what each certificate is
// (self-issued means root, anything else means intermediate) rather than by where it sits
// in the file. So reordering or re-wrapping the file is safe by design, and the property
// says so rather than leaving the next editor to hope. Substituting a certificate is the
// case that must never pass for the anchor, and it is stated negatively for the usual
// reason: a trust root that wrongly refuses is noticed the same day, and one that wrongly
// accepts is noticed after it has been used.
func TestPropertyTheAnchorIsItsContent(t *testing.T) {
	t.Parallel()
	pinned := fingerprints(certsOf(t, trustanchor.Fulcio))

	t.Run("reordering and re-wrapping the file changes nothing", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(rt *rapid.T) {
			certs := certsOf(t, trustanchor.Fulcio)
			shuffled := rapid.Permutation(certs).Draw(rt, "order")
			between := rapid.SampledFrom([]string{
				"", "\n", "\r\n",
				"Fulcio root, do not edit by hand\n",
				"subject=CN=sigstore,O=sigstore.dev\n\n",
			}).Draw(rt, "between blocks")

			got := fingerprints(certsOf(t, emit(shuffled, between)))
			if !slices.Equal(got, pinned) {
				rt.Fatalf("re-emitting the chain changed what it pins:\n got %v\nwant %v", got, pinned)
			}
		})
	})

	t.Run("no substituted authority is ever the anchor", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(rt *rapid.T) {
			certs := certsOf(t, trustanchor.Fulcio)
			// Replace one of the pinned certificates with a CA of the attacker's own
			// making, or drop it entirely. Either way the bundle still looks like a
			// perfectly well-formed Fulcio chain to anything that only checks its shape.
			victim := rapid.IntRange(0, len(certs)-1).Draw(rt, "replaced")
			if rapid.Bool().Draw(rt, "substitute rather than drop") {
				certs[victim] = mintCA(rt, certs[victim].Subject.CommonName)
			} else {
				certs = slices.Delete(certs, victim, victim+1)
			}

			if got := fingerprints(certsOf(t, emit(certs, ""))); slices.Equal(got, pinned) {
				rt.Fatal("a chain with a substituted authority compared equal to the anchor")
			}
		})
	})
}

// mintCA issues a self-signed CA carrying whatever name it is given. This is the attacker
// who cannot compromise Sigstore but can certainly generate a key and call themselves
// sigstore.dev.
func mintCA(rt *rapid.T, commonName string) *x509.Certificate {
	rt.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		rt.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"sigstore.dev"}},
		NotBefore:             time.Date(2021, 10, 7, 13, 56, 59, 0, time.UTC),
		NotAfter:              time.Date(2031, 10, 5, 13, 56, 58, 0, time.UTC),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		rt.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		rt.Fatal(err)
	}
	return c
}
