package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Hash returns the stable content hash of r: a hex SHA-256 over its canonical
// content (identity, scope, labels and annotations, spec, status, valid-time, and
// the tombstone flag), excluding the volatile envelope fields (versions, clocks,
// timestamps, and the hash itself). Equal content yields an equal hash on any
// machine, so resource history forms a Merkle DAG: dedup, provenance ("which
// version produced this"), tamper-evidence, and efficient diff-based sync.
//
// Spec and Status are canonicalized (re-encoded with sorted keys) so two
// semantically equal specs that differ only in key order or whitespace hash the
// same.
func Hash(r Resource) (string, error) {
	return contentHash(r, canonicalJSON(r.Spec))
}

func contentHash(r Resource, canonicalSpec any) (string, error) {
	content := map[string]any{
		"apiVersion":  r.APIVersion,
		"kind":        r.Kind,
		"name":        r.Name,
		"scope":       r.Scope,
		"labels":      r.Labels,
		"annotations": r.Annotations,
		"spec":        canonicalSpec,
		"status":      canonicalJSON(r.Status),
		"deleted":     r.Deleted,
		"validFrom":   r.ValidFrom,
		"validTo":     r.ValidTo,
	}
	b, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// SpecHash returns a stable hash of a resource's desired state alone (its kind and
// canonical spec), excluding status and all envelope metadata. It is the agent's
// equivalent of Kubernetes' metadata.generation: a controller records the SpecHash
// it last acted on in status, and a reconcile is a no-op while the stored spec hash
// still matches, so writing status (which changes the full content hash) never
// re-triggers work. Equal spec yields an equal hash on any machine.
func SpecHash(r Resource) (string, error) {
	return specHash(r, canonicalJSON(r.Spec))
}

func specHash(r Resource, canonicalSpec any) (string, error) {
	desired := map[string]any{
		"apiVersion": r.APIVersion,
		"kind":       r.Kind,
		"spec":       canonicalSpec,
	}
	b, err := json.Marshal(desired)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Hashes returns Hash and SpecHash together, canonicalizing the spec once. The
// Stamper stamps both onto every write, so readers (the reconciler's no-op check
// above all) compare stored fields instead of re-canonicalizing per tick.
func Hashes(r Resource) (content, spec string, err error) {
	cs := canonicalJSON(r.Spec)
	content, err = contentHash(r, cs)
	if err != nil {
		return "", "", err
	}
	spec, err = specHash(r, cs)
	if err != nil {
		return "", "", err
	}
	return content, spec, nil
}

// canonicalJSON decodes raw JSON to a generic value so the outer Marshal re-encodes
// it with sorted object keys (Go marshals map[string]any deterministically). Empty
// input is null; input that is not valid JSON is hashed as an opaque string.
func canonicalJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	return v
}
