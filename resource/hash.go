package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Hash returns the hex SHA-256 of r's canonical content (identity, scope, labels,
// annotations, spec, status, valid-time, tombstone flag), excluding volatile envelope
// fields. Equal content hashes equal on any machine, so resource history is a Merkle
// DAG usable for dedup, provenance, tamper evidence and diff-based sync.
//
// Spec is re-encoded with sorted keys, so key order and whitespace do not count.
// Status is folded in as a digest of its bytes: one controller writes it through a
// stable encoder and Merge never re-hashes a remote status, so bytes already equal
// meaning, and a digest keeps a write's allocation flat however large the status
// grows (a goal embeds its transcript).
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
		"status":      statusDigest(r.Status),
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

// statusDigest returns a hex SHA-256 over the status bytes, a constant-cost stand-in
// that does not grow with an embedded transcript. Empty status hashes to the digest
// of no bytes, distinct from any non-empty status.
func statusDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SpecHash returns a stable hash of kind and canonical spec alone, the agent's
// metadata.generation. A controller records the SpecHash it acted on in status, and
// reconcile is a no-op while it matches, so a status write never re-triggers work.
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

// Hashes returns Hash and SpecHash, canonicalizing the spec once. The Stamper
// stamps both on every write, so the reconciler's no-op check compares fields.
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

// canonicalJSON decodes raw JSON to a generic value so Marshal re-encodes it with
// sorted keys. Empty input is null; invalid JSON is hashed as an opaque string.
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
