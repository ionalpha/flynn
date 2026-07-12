// Package release verifies that a flynn release artifact is genuinely one this
// project built, using only the evidence the release itself carries and trust
// anchors compiled into the binary.
//
// A release publishes a Sigstore bundle: a DSSE envelope whose payload is a SLSA
// provenance statement naming every artifact and its sha256, signed by a short-lived
// Fulcio certificate that GitHub's OIDC provider issued to the release workflow, and
// recorded in the Rekor transparency log with an inclusion proof and a signed
// checkpoint. There is no long-lived signing key to steal: the certificate is the
// identity, and it lives for ten minutes.
//
// Verification is offline and self-contained. It takes the bundle bytes and answers
// one question: which artifacts, by digest, did the flynn release workflow build at
// which tag? Nothing here downloads, installs, or executes anything. A caller pins a
// download to the digest this package returns, so a hostile mirror, a hijacked
// release asset, or a perfect TLS interception still cannot substitute a binary: TLS
// is not the trust anchor, the signature is.
//
// The checks, in the order a forgery would have to defeat them:
//
//   - The certificate chains to the embedded Fulcio roots, checked at the time Rekor
//     recorded the entry rather than at "now": these certificates are valid for ten
//     minutes, so a genuine release always fails a present-time check.
//   - The certificate's identity is pinned to this project's release workflow at a
//     version tag, by numeric repository and owner id as well as by URL, so renaming
//     a repository, transferring it, or building the same path in a fork does not
//     produce an accepted identity.
//   - The DSSE signature verifies against that certificate's key.
//   - The Rekor entry's inclusion proof verifies to a checkpoint signed by the
//     embedded log key, and that entry commits to this exact envelope. A signature
//     that was never publicly logged is not accepted, so a forgery cannot be issued
//     in the dark: it has to be published in a log the world can read.
package release

import (
	"time"

	"github.com/ionalpha/flynn/fault"
)

// Failure codes. They are stable identifiers, so a caller (or an operator reading an
// event log) can tell a malformed bundle apart from a bundle that is well-formed but
// signed by someone who is not us.
const (
	CodeBundleDecode   = "release.bundle_decode"
	CodeCertificate    = "release.certificate"
	CodeIdentity       = "release.identity"
	CodeSignature      = "release.signature"
	CodeTransparency   = "release.transparency"
	CodeStatement      = "release.statement"
	CodeTrustRoot      = "release.trust_root"
	CodeNoSuchArtifact = "release.no_such_artifact"
	CodeUnexpectedTag  = "release.unexpected_tag"
)

// Provenance is what a verified bundle proves: the flynn release workflow, running
// in this repository at this tag and commit, produced exactly these artifacts.
type Provenance struct {
	// Tag is the git tag the release was built from ("v0.1.3").
	Tag string
	// Commit is the commit the workflow checked out.
	Commit string
	// Artifacts maps a release asset's file name to its lowercase hex sha256.
	Artifacts map[string]string
	// SignerIdentity is the certificate identity that signed, which is the workflow
	// URI at the ref. It is shown to the operator so the trust decision is legible
	// rather than implicit.
	SignerIdentity string
	// LogIndex and LoggedAt place the signature in the public transparency log, so an
	// operator (or an auditor replaying the record) can go look the entry up.
	LogIndex int64
	LoggedAt time.Time
}

// Digest returns the verified sha256 of one named artifact.
func (p Provenance) Digest(artifact string) (string, error) {
	d, ok := p.Artifacts[artifact]
	if !ok || d == "" {
		return "", fault.New(fault.Terminal, CodeNoSuchArtifact,
			"the signed provenance for "+p.Tag+" does not cover an artifact named "+artifact)
	}
	return d, nil
}

// Verify checks a Sigstore bundle end to end and reports what it proves. It is the
// only entry point: there is deliberately no way to parse a bundle without verifying
// it, so no caller can read an attacker-controlled artifact list by accident.
func Verify(bundleJSON []byte) (Provenance, error) {
	return verifyWith(bundleJSON, defaultPolicy(), embeddedTrustRoot)
}

// policy is the identity a signature must present to be believed. It is not
// configurable from outside the package: a flag that relaxes who may sign flynn's
// releases is a vulnerability with a command-line interface.
type policy struct {
	repositoryURI  string
	workflowPath   string
	repositoryID   string
	ownerID        string
	runnerEnv      string
	buildTrigger   string
	oidcIssuer     string
	tagRefPrefix   string
	checkpointName string
}

func defaultPolicy() policy {
	return policy{
		repositoryURI:  "https://github.com/ionalpha/flynn",
		workflowPath:   ".github/workflows/release.yml",
		repositoryID:   "1279616876",
		ownerID:        "127713176",
		runnerEnv:      "github-hosted",
		buildTrigger:   "push",
		oidcIssuer:     "https://token.actions.githubusercontent.com",
		tagRefPrefix:   "refs/tags/v",
		checkpointName: "rekor.sigstore.dev",
	}
}

// verifyWith is Verify with the policy and trust root injected, so a test can prove
// that a bundle signed by the wrong identity, or logged in a different log, is
// rejected: the negative cases are the ones worth testing.
func verifyWith(bundleJSON []byte, p policy, root *trustRoot) (Provenance, error) {
	b, err := decodeBundle(bundleJSON)
	if err != nil {
		return Provenance{}, err
	}

	entry, err := b.soleTLogEntry()
	if err != nil {
		return Provenance{}, err
	}

	// The certificate is checked as of the moment the log recorded the entry. Fulcio
	// issues ten-minute certificates, so "is it valid now" is the wrong question and
	// would reject every release older than ten minutes.
	leaf, err := root.verifyChain(b.leafCertificate, entry.integratedTime)
	if err != nil {
		return Provenance{}, err
	}

	ident, err := p.checkIdentity(leaf)
	if err != nil {
		return Provenance{}, err
	}

	if err := b.verifyDSSESignature(leaf); err != nil {
		return Provenance{}, err
	}

	// The signature is only believed once it is in the public log: the inclusion proof
	// binds this envelope to a checkpoint the log operator signed.
	if err := root.verifyInclusion(entry, b, p.checkpointName); err != nil {
		return Provenance{}, err
	}

	stmt, err := b.statement()
	if err != nil {
		return Provenance{}, err
	}
	if err := p.checkStatement(stmt, ident); err != nil {
		return Provenance{}, err
	}

	arts, err := stmt.artifacts()
	if err != nil {
		return Provenance{}, err
	}

	return Provenance{
		Tag:            ident.tag,
		Commit:         ident.commit,
		Artifacts:      arts,
		SignerIdentity: ident.signer,
		LogIndex:       entry.logIndex,
		LoggedAt:       entry.integratedTime,
	}, nil
}
