# Test fixtures

These three files are the real `checksums.txt`, signature and certificate from the `token/v0.1.0` release of `ionalpha/flynn-extensions`, not synthetic material. `sigstore_test.go` verifies against them so the test proves this package agrees with Sigstore, rather than proving it agrees with a fixture it generated itself.

`checksums.txt.pem` is base64 wrapping a single certificate, which is the form the release publishes. It is a Fulcio short-lived keyless signing certificate, issued by `CN=sigstore-intermediate`, valid from 2026-07-12 02:13:05 to 02:23:05 UTC. That ten-minute window closed long ago and cannot be reopened.

Nothing here is a secret. Keyless signing binds an identity to a certificate for the length of one signing operation and holds no private key afterwards, and the certificate is written to the public Rekor transparency log at the moment it is issued. Its only content is the workflow that signed, `https://github.com/ionalpha/go-ci/.github/workflows/monorepo-release.yml@refs/heads/main`, recorded as a critical SAN URI.

The expiry does not date the fixture, because `checkChain` verifies at `cert.NotBefore` rather than at the wall clock. Every keyless certificate is already expired by the time anyone verifies a release with it; what makes it trustworthy is the transparency log entry, not an unexpired validity window.

Replacing these fixtures means taking all three files from one release together. The signature is over the exact bytes of `checksums.txt`, and the certificate is the only thing it verifies under.
