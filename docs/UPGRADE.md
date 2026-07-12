# Upgrading flynn

One installed binary is all you need. flynn can tell you what it is, what else exists,
and replace itself with a newer release that it has verified for itself.

```
flynn version           # what am I running, and from where
flynn version list      # what releases exist
flynn version check     # is there a newer one (exits non-zero if yes)
flynn upgrade           # install the newest release
```

`flynn upgrade` shows you what it verified before it installs anything:

```
$ flynn upgrade
0.1.2 -> v0.1.3
  signed by  https://github.com/ionalpha/flynn/.github/workflows/release.yml@refs/tags/v0.1.3
  built from 22dba388133eab33e7c9fc5b8fe8b50029e1d9d1
  logged at  rekor.sigstore.dev index 2112785086 (2026-07-08T06:32:18Z)
  artifact   flynn_linux_amd64.tar.gz
  sha256     a39c486e14442bef6144e202bacbcd9d3f2ec9c4e03f9297b1ee78848e87a487
  installs   /home/you/.local/bin/flynn
Install v0.1.3? [y/N]
```

Useful flags: `--to vX.Y.Z` for an exact version, `--pre` to consider prereleases,
`--check` to see the plan without installing, `--yes` for unattended runs, and
`--allow-downgrade` for the rare case where going back is what you actually want.

## What is checked, and why you can believe it

Nothing that is downloaded is trusted until it has been proven, and the proof does not
depend on the connection it arrived over. TLS is not the trust anchor here: the
signature is. Someone who controls the network, a certificate authority, or the mirror
serving the file still cannot get a binary past these checks.

Every release is signed at build time by GitHub's OIDC identity for flynn's release
workflow, using a Fulcio certificate that exists for ten minutes and is then gone, and
the signature is published in Rekor, the public transparency log. There is no long-lived
signing key anywhere in this project, so there is no signing key for anyone to steal.

When you upgrade, this binary:

1. Verifies the release's signing certificate chains to the Sigstore certificate
   authority compiled into it, **as of the moment the transparency log recorded the
   signature**. The certificate is long expired by the time you install; that is the
   design, and checking it against "now" would be wrong.
2. Requires the certificate's identity to be flynn's own release workflow, running on a
   version tag, in this repository, checked by numeric repository and owner id as well
   as by URL. A fork, a renamed repository, a branch build, or a different workflow in
   this same repository all fail here, even though every one of them can obtain a
   perfectly genuine Sigstore certificate.
3. Verifies the signature over the SLSA provenance statement that names every artifact
   in the release and its sha256.
4. Verifies that the signature is in the public transparency log: the inclusion proof
   has to reconstruct a root hash that the log operator signed a checkpoint over, and
   the logged entry has to commit to this exact payload, signature, and certificate.
   **A signature that was never published is not accepted**, so nobody can forge one
   quietly: a forgery has to be entered in a log the whole world can read.
5. Only then downloads the archive, pinned to the sha256 the signed provenance gives it,
   verified as the bytes arrive.

The release listing (which versions exist) is the one thing in this path that is not
signed, and it is treated accordingly: it is used to decide what to go and check, never
as evidence. To keep a hostile listing from being useful, flynn remembers the highest
version it has ever verified and the newest it has ever been offered. It refuses to
install anything older than the former without `--allow-downgrade`, and it tells you
when a listing stops offering a release it used to.

## What happens on disk

The new binary is staged in the same directory as the one it replaces, so the swap is a
rename within a single filesystem and is therefore atomic: at no point does the path
hold a half-written file. The running binary's path is resolved through its symlinks
first, so the write lands on the file that is really running, and no symlink planted in
the meantime can redirect it. The new binary is made to run and report its own version
before it is kept. If it will not run, the old one stays exactly where it was.

Windows cannot overwrite a running executable, but it can rename one, so the outgoing
binary is moved aside and the new one takes its place; if that second step fails, the
original is put straight back. The displaced file is swept on the next start.

## When flynn will refuse to upgrade itself

- **This flynn was installed by a package manager** (a distribution package, Homebrew,
  Nix, snap, Chocolatey, `go install`). Overwriting the file would leave the manager's
  records describing something that is no longer there, and its next upgrade would
  silently undo this one. Use the tool that installed it.
- **This flynn was built from source.** There is no released version to be newer than,
  and upgrading would throw away your own build.
- **The directory is not writable by you.** flynn will not try to escalate its own
  privileges to overwrite itself, which is exactly the behaviour you want from a program
  that is about to replace its own executable.

## Verifying a release yourself

You do not have to take flynn's word for any of this. Every release ships the same
bundle flynn checks (`flynn.intoto.jsonl`), and it verifies with the standard tooling:

```
gh attestation verify flynn_linux_amd64.tar.gz --repo ionalpha/flynn
```
