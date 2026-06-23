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
	content := map[string]any{
		"apiVersion":  r.APIVersion,
		"kind":        r.Kind,
		"name":        r.Name,
		"scope":       r.Scope,
		"labels":      r.Labels,
		"annotations": r.Annotations,
		"spec":        canonicalJSON(r.Spec),
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
