package release

import (
	"bytes"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// The Fulcio certificate extensions that carry who built the artifact. The
// short-lived signing certificate carries no name; these are the identity, asserted by
// the certificate authority from GitHub's OIDC token.
var (
	oidIssuer         = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
	oidBuildSignerURI = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 9}
	oidRunnerEnv      = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 11}
	oidSourceRepoURI  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 12}
	oidSourceRepoSHA  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 13}
	oidSourceRepoRef  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 14}
	oidSourceRepoID   = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 15}
	oidOwnerID        = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 17}
	oidBuildTrigger   = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 20}
)

// identity is the verified answer to "who signed this, building what, from where".
type identity struct {
	signer string // the workflow URI at the ref that signed
	tag    string // "v0.1.3"
	ref    string // "refs/tags/v0.1.3"
	commit string
}

// checkIdentity refuses any certificate that is not this project's release workflow
// running on a version tag.
//
// The numeric repository and owner ids are checked with the URLs because a renamed
// repository's old name can be claimed by someone else, and a URL-only check would
// accept the impostor's workflow; the ids never move. A tag ref is required, so a
// signature from a branch, a pull request or a fork's workflow_dispatch is no
// release, however genuine its certificate.
func (p policy) checkIdentity(leaf *x509.Certificate) (identity, error) {
	if len(leaf.URIs) != 1 {
		return identity{}, fault.New(fault.Terminal, CodeIdentity,
			fmt.Sprintf("the signing certificate names %d identities; exactly one is expected", len(leaf.URIs)))
	}
	san := leaf.URIs[0].String()

	ext, err := fulcioExtensions(leaf)
	if err != nil {
		return identity{}, err
	}

	// The signer URI is the workflow that ran, at the ref it ran on. It must be this
	// project's release workflow, and the subject alternative name must agree with it,
	// so there is no second identity hiding behind the first.
	signer := ext[oidBuildSignerURI.String()]
	if signer != san {
		return identity{}, fault.New(fault.Terminal, CodeIdentity,
			"the signing certificate's subject and its build signer disagree about who signed")
	}
	ref := ext[oidSourceRepoRef.String()]
	wantSigner := p.repositoryURI + "/" + p.workflowPath + "@" + ref
	if signer != wantSigner {
		return identity{}, fault.New(fault.Terminal, CodeIdentity,
			"the release was signed by "+signer+", not by this project's release workflow")
	}

	for _, want := range []struct {
		oid, value, what string
	}{
		{oidIssuer.String(), p.oidcIssuer, "identity provider"},
		{oidSourceRepoURI.String(), p.repositoryURI, "source repository"},
		{oidSourceRepoID.String(), p.repositoryID, "source repository id"},
		{oidOwnerID.String(), p.ownerID, "repository owner id"},
		{oidRunnerEnv.String(), p.runnerEnv, "runner environment"},
		{oidBuildTrigger.String(), p.buildTrigger, "build trigger"},
	} {
		if got := ext[want.oid]; got != want.value {
			return identity{}, fault.New(fault.Terminal, CodeIdentity,
				fmt.Sprintf("the release was signed with an unexpected %s (%q, expected %q)", want.what, got, want.value))
		}
	}

	// Only a version tag makes a release. A branch build carries the same repository
	// and the same workflow, and accepting one would let anyone with push access to a
	// branch mint something this binary would install.
	if !strings.HasPrefix(ref, p.tagRefPrefix) {
		return identity{}, fault.New(fault.Terminal, CodeIdentity,
			"the release was signed on "+ref+", which is not a version tag")
	}
	tag := strings.TrimPrefix(ref, "refs/tags/")
	if strings.ContainsAny(tag, "/\\") || tag == "" {
		return identity{}, fault.New(fault.Terminal, CodeIdentity, "the release tag "+tag+" is not a plain version tag")
	}

	commit := ext[oidSourceRepoSHA.String()]
	if len(commit) != 40 || strings.Trim(commit, "0123456789abcdef") != "" {
		return identity{}, fault.New(fault.Terminal, CodeIdentity, "the release names no source commit")
	}

	return identity{signer: signer, tag: tag, ref: ref, commit: commit}, nil
}

// fulcioExtensions reads the certificate's identity extensions, DER-encoded UTF8
// strings in the current set. The pre-2023 extensions carried bare strings, and a
// certificate mixing the two is refused.
func fulcioExtensions(leaf *x509.Certificate) (map[string]string, error) {
	out := make(map[string]string, 8)
	for _, e := range leaf.Extensions {
		if len(e.Id) < 7 || e.Id[6] != 57264 {
			continue
		}
		var s string
		rest, err := asn1.Unmarshal(e.Value, &s)
		if err != nil || len(rest) != 0 {
			// A legacy bare-string extension; only the DER set is read.
			continue
		}
		out[e.Id.String()] = s
	}
	if len(out) == 0 {
		return nil, fault.New(fault.Terminal, CodeIdentity,
			"the signing certificate carries no build identity, so there is no way to tell who produced this release")
	}
	return out, nil
}

// certPEMMatches reports whether a PEM-encoded certificate is the same certificate as
// the given DER bytes.
func certPEMMatches(certPEM, der []byte) bool {
	blk, _ := pem.Decode(certPEM)
	return blk != nil && blk.Type == "CERTIFICATE" && bytes.Equal(blk.Bytes, der)
}
