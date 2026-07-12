package release

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/fault"
)

// The in-toto and SLSA types this verifier reads. Anything else is refused: a
// statement whose meaning this code does not know is a statement it cannot check.
const (
	statementType = "https://in-toto.io/Statement/v1"
	predicateType = "https://slsa.dev/provenance/v1"
	buildType     = "https://actions.github.io/buildtypes/workflow/v1"
)

// maxArtifacts caps how many artifacts one release may claim, so a signed but
// malformed statement cannot make the verifier build an unbounded map.
const maxArtifacts = 256

// statement is the signed payload: a SLSA provenance statement listing what the build
// produced and how it was built.
type statement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	Predicate struct {
		BuildDefinition struct {
			BuildType          string `json:"buildType"`
			ExternalParameters struct {
				Workflow struct {
					Ref        string `json:"ref"`
					Repository string `json:"repository"`
					Path       string `json:"path"`
				} `json:"workflow"`
			} `json:"externalParameters"`
		} `json:"buildDefinition"`
	} `json:"predicate"`
}

func (b *bundle) statement() (*statement, error) {
	if b.DSSEEnvelope.PayloadType != "application/vnd.in-toto+json" {
		return nil, fault.New(fault.Terminal, CodeStatement,
			"the signed payload is of type "+b.DSSEEnvelope.PayloadType+", which this verifier does not understand")
	}
	var s statement
	if err := json.Unmarshal(b.payload, &s); err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeStatement, fmt.Errorf("reading the signed provenance: %w", err))
	}
	if s.Type != statementType || s.PredicateType != predicateType {
		return nil, fault.New(fault.Terminal, CodeStatement,
			"the signed payload is not a SLSA provenance statement")
	}
	return &s, nil
}

// checkStatement holds the signed provenance to the same story the certificate tells.
// The certificate says which workflow signed; the provenance says which workflow
// built. If those two disagree, one of them is describing a different build, and this
// verifier does not get to choose which one to believe.
func (p policy) checkStatement(s *statement, id identity) error {
	bd := s.Predicate.BuildDefinition
	if bd.BuildType != buildType {
		return fault.New(fault.Terminal, CodeStatement,
			"the release was produced by an unexpected build type ("+bd.BuildType+")")
	}
	w := bd.ExternalParameters.Workflow
	if w.Repository != p.repositoryURI || w.Path != p.workflowPath {
		return fault.New(fault.Terminal, CodeStatement,
			"the signed provenance describes a build of "+w.Repository+" by "+w.Path+", which is not this project's release workflow")
	}
	if w.Ref != id.ref {
		return fault.New(fault.Terminal, CodeStatement,
			"the signed provenance was built from "+w.Ref+" but signed on "+id.ref)
	}
	return nil
}

// artifacts maps each named artifact to the sha256 the signed provenance gives it.
// This map is the whole point of the verification: a caller pins its download to a
// digest from here, so what lands on disk is what this build produced or nothing is.
func (s *statement) artifacts() (map[string]string, error) {
	if len(s.Subject) == 0 {
		return nil, fault.New(fault.Terminal, CodeStatement, "the signed provenance covers no artifacts")
	}
	if len(s.Subject) > maxArtifacts {
		return nil, fault.New(fault.Terminal, CodeStatement,
			fmt.Sprintf("the signed provenance covers %d artifacts, over the %d ceiling", len(s.Subject), maxArtifacts))
	}

	out := make(map[string]string, len(s.Subject))
	for _, sub := range s.Subject {
		// A name that is not a plain file name has no business being matched against a
		// release asset, and a path separator in one is how a name gets to mean a
		// different file than it appears to.
		if sub.Name == "" || strings.ContainsAny(sub.Name, `/\`) {
			return nil, fault.New(fault.Terminal, CodeStatement,
				"the signed provenance names an artifact that is not a plain file name: "+sub.Name)
		}
		d := strings.ToLower(sub.Digest["sha256"])
		if len(d) != 64 || strings.Trim(d, "0123456789abcdef") != "" {
			return nil, fault.New(fault.Terminal, CodeStatement,
				"the signed provenance gives artifact "+sub.Name+" no usable sha256")
		}
		// Two subjects with the same name and different digests would leave the choice of
		// which one to install to map iteration order.
		if prev, dup := out[sub.Name]; dup && prev != d {
			return nil, fault.New(fault.Terminal, CodeStatement,
				"the signed provenance gives artifact "+sub.Name+" two different digests")
		}
		out[sub.Name] = d
	}
	return out, nil
}
