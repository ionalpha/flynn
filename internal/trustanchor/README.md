# Pinned trust anchors

Both files in `trust/` are published anchors from the Sigstore public-good instance. Neither holds a secret or a credential of ours, so there is nothing here to rotate or revoke on our side. They are committed and embedded with `go:embed`: a trust root fetched at verification time would be only as trustworthy as the connection it arrived over.

`trust/fulcio.pem` holds two certificates and no key block. Together they are the Fulcio CA chain that issues release signing certificates.

| Subject | Valid |
| --- | --- |
| `O=sigstore.dev, CN=sigstore` (root, self-signed) | 2021-10-07 to 2031-10-05 |
| `O=sigstore.dev, CN=sigstore-intermediate` | 2022-04-13 to 2031-10-05 |

`trust/rekor.pub` is the ECDSA P-256 public key of the Rekor transparency log. The SHA-256 of its DER encoding is `c0d23d6ad406973f9559f3ba2d1ca01f84147d8ffc5b8445c224f98b9591801d`, which is the log id carried in every Sigstore bundle flynn publishes, and `loadTrustRoot` in `internal/release` enforces that correspondence at startup.

## Why the anchors have a package of their own

Two packages verify against them. `internal/release` checks flynn's own releases and needs both files; `internal/sigstore` checks extension releases and needs the Fulcio chain. Neither imports the other. Keeping one copy here means a rotation updates both verification paths at once; with a copy in each package, updating one would leave the other pinned to a retired CA while every test still passed.

## Updating them

Take the chain from the Sigstore public-good TUF root rather than from a mirror, replace `trust/fulcio.pem` whole, and check that `TestEmbeddedRootsAreTheRealOnes` in `internal/sigstore` and the trust-root tests in `internal/release` still pass. Changing `trust/rekor.pub` also changes the log id, so any bundle signed before the change stops verifying; that is a release-wide decision, not a file edit.
