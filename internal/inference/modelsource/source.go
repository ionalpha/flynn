// Package modelsource classifies where a model's weights come from and how far that
// source can be trusted, so a model from anywhere (a curated catalog entry, a model-hub
// reference, a raw URL, or a file a user drops in) goes through one trust decision before
// it is ever fetched or run. A model file is untrusted input to a parser with a history
// of memory-safety flaws, so the source it came from sets the containment a run requires:
// our own pinned catalog entry is the trust anchor, a recognized publisher is
// semi-trusted, and anything else is untrusted by default and may only run where it can
// be genuinely contained.
//
// The package is pure: it parses a reference, names the weight format, and maps the
// source to a trust level. It performs no I/O, so the classification is fully testable
// and is the same whatever calls it. Fetching, pinning, and running compose on top.
package modelsource

import (
	"path"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/sandbox"
)

// Kind is where a model's weights come from. The kind alone does not decide trust (a
// publisher still matters), but it decides how the reference is resolved to bytes.
type Kind int

const (
	// KindCatalog is a blessed entry in the embedded catalog: pinned URL and digest,
	// vetted offline. The catalog is the trust anchor, independent of any live registry.
	KindCatalog Kind = iota
	// KindHuggingFace is a model-hub reference, hf:owner/repo[/file] or a huggingface.co
	// URL. The owner is the publisher whose reputation the trust decision turns on.
	KindHuggingFace
	// KindURL is a direct https link to a weights file from no recognized publisher.
	KindURL
	// KindFile is a local weights file a user points at or drops in.
	KindFile
)

// Source is a parsed, not-yet-fetched model reference. Exactly the fields for its Kind
// are populated.
type Source struct {
	// Kind is how the reference resolves to bytes.
	Kind Kind
	// Raw is the original reference string, kept for provenance and messages.
	Raw string
	// CatalogID is the catalog entry id, when KindCatalog.
	CatalogID string
	// Owner and Repo identify a hub model, when KindHuggingFace. File is the optional
	// specific weights file within the repo.
	Owner, Repo, File string
	// URL is the direct download location, when KindURL.
	URL string
	// Path is the local file path, when KindFile.
	Path string
}

// Parse classifies a reference string into a Source. isCatalogID reports whether a bare
// string names a known catalog entry, so a catalog id is recognized before it is treated
// as anything else. The grammar is deliberately small and unambiguous: a catalog id, an
// hf: prefix or a huggingface.co URL, any other https URL, or a local path.
func Parse(ref string, isCatalogID func(string) bool) (Source, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Source{}, fault.New(fault.Terminal, "modelsource_empty", "model source: empty reference")
	}
	if isCatalogID != nil && isCatalogID(ref) {
		return Source{Kind: KindCatalog, Raw: ref, CatalogID: ref}, nil
	}
	if rest, ok := strings.CutPrefix(ref, "hf:"); ok {
		return parseHuggingFace(ref, rest)
	}
	if strings.HasPrefix(ref, "https://huggingface.co/") {
		return parseHuggingFaceURL(ref)
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return Source{Kind: KindURL, Raw: ref, URL: ref}, nil
	}
	// Anything else is treated as a local file path the user is pointing at.
	return Source{Kind: KindFile, Raw: ref, Path: ref}, nil
}

// parseHuggingFace reads an hf:owner/repo[/file] reference.
func parseHuggingFace(raw, rest string) (Source, error) {
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Source{}, fault.New(fault.Terminal, "modelsource_bad_hf",
			"model source: a hub reference must be hf:owner/repo[/file], got "+raw)
	}
	s := Source{Kind: KindHuggingFace, Raw: raw, Owner: parts[0], Repo: parts[1]}
	if len(parts) == 3 {
		s.File = parts[2]
	}
	return s, nil
}

// parseHuggingFaceURL reads a huggingface.co URL into the same shape as an hf: reference,
// so a pasted URL and a typed reference classify identically.
func parseHuggingFaceURL(raw string) (Source, error) {
	rest := strings.TrimPrefix(raw, "https://huggingface.co/")
	rest = strings.TrimSuffix(rest, "/")
	// A resolve URL embeds the file after /resolve/<rev>/; keep it as the file, drop the
	// revision marker so owner/repo stay clean.
	if i := strings.Index(rest, "/resolve/"); i >= 0 {
		ownerRepo := rest[:i]
		after := rest[i+len("/resolve/"):]
		if slash := strings.Index(after, "/"); slash >= 0 {
			after = after[slash+1:]
		}
		parts := strings.SplitN(ownerRepo, "/", 2)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return Source{}, fault.New(fault.Terminal, "modelsource_bad_hf_url", "model source: malformed huggingface URL "+raw)
		}
		return Source{Kind: KindHuggingFace, Raw: raw, Owner: parts[0], Repo: parts[1], File: after}, nil
	}
	return parseHuggingFace(raw, rest)
}

// Key is a stable identifier for this source, used to record provenance and to pin its
// integrity on first use. Two references that resolve to the same weights share a key.
func (s Source) Key() string {
	switch s.Kind {
	case KindCatalog:
		return "catalog:" + s.CatalogID
	case KindHuggingFace:
		k := "hf:" + s.Owner + "/" + s.Repo
		if s.File != "" {
			k += "/" + s.File
		}
		return k
	case KindURL:
		return "url:" + s.URL
	case KindFile:
		return "file:" + s.Path
	default:
		return s.Raw
	}
}

// WeightFormat is the on-disk encoding of a weights file, which decides whether loading
// it can execute code.
type WeightFormat int

const (
	// FormatUnknown is an extension we do not recognize. It is refused, since an
	// unrecognized container cannot be assumed safe to parse.
	FormatUnknown WeightFormat = iota
	// FormatGGUF is the single-file quantized format, read by the hardened reader.
	FormatGGUF
	// FormatSafetensors stores tensors only and executes no code on load.
	FormatSafetensors
	// FormatCodeExecuting is a format that can run arbitrary code when loaded (a Python
	// pickle, or an archive that could carry one). It is refused everywhere.
	FormatCodeExecuting
)

// DetectFormat names the weight format from a filename or path by its extension. An empty
// or extension-less name is unknown, not assumed safe.
func DetectFormat(name string) WeightFormat {
	switch strings.ToLower(path.Ext(name)) {
	case ".gguf":
		return FormatGGUF
	case ".safetensors":
		return FormatSafetensors
	case ".bin", ".pt", ".pth", ".ckpt", ".pkl", ".pickle", ".zip", ".tar", ".tgz", ".gz", ".7z", ".rar", ".npz":
		return FormatCodeExecuting
	default:
		return FormatUnknown
	}
}

// CheckRunnableFormat refuses a format that cannot be safely parsed: a code-executing
// format outright, and an unrecognized one conservatively. Only the safe-parse formats
// (GGUF, safetensors) are allowed. This holds for every source, not only catalog fetches,
// so a dropped pickle file is refused the same as a downloaded one.
func CheckRunnableFormat(name string) error {
	switch DetectFormat(name) {
	case FormatGGUF, FormatSafetensors:
		return nil
	case FormatCodeExecuting:
		return fault.New(fault.Forbidden, "modelsource_codeexec_format",
			"model source: "+name+" uses a code-executing weight format and will not be loaded; use a gguf or safetensors file")
	default:
		return fault.New(fault.Forbidden, "modelsource_unknown_format",
			"model source: "+name+" has an unrecognized weight format; only gguf and safetensors are allowed")
	}
}

// Classification is the trust decision for a source: the containment-setting trust level
// and a plain-language reason a user can be shown.
type Classification struct {
	// Trust is the sandbox trust level the source maps to, which sets the containment a
	// run requires through sandbox.Required.
	Trust sandbox.Trust
	// Reason is a short, plain-language explanation of why the source got this trust.
	Reason string
}

// Classify maps a source to a trust level. The embedded catalog is the trust anchor, so a
// catalog entry is trusted. A hub model from a recognized publisher is semi-trusted: the
// publisher is reputable but the bytes are still parsed by a vulnerable runtime, so it
// needs kernel confinement. Everything else (an unknown publisher, a raw URL, a local
// file) is untrusted by default and may run only where it can be genuinely contained.
// knownPublisher reports whether a hub owner is a recognized first-party publisher.
func Classify(s Source, knownPublisher func(owner string) bool) Classification {
	switch s.Kind {
	case KindCatalog:
		return Classification{Trust: sandbox.TrustTrusted, Reason: "a vetted, digest-pinned catalog model"}
	case KindHuggingFace:
		if knownPublisher != nil && knownPublisher(s.Owner) {
			return Classification{Trust: sandbox.TrustSemi, Reason: "a model from the recognized publisher " + s.Owner}
		}
		return Classification{Trust: sandbox.TrustUntrusted, Reason: "a model from the unrecognized publisher " + s.Owner}
	case KindURL:
		return Classification{Trust: sandbox.TrustUntrusted, Reason: "a model from a direct URL with no verified publisher"}
	case KindFile:
		return Classification{Trust: sandbox.TrustUntrusted, Reason: "a local model file of unverified origin"}
	default:
		return Classification{Trust: sandbox.TrustUntrusted, Reason: "a model of unknown source"}
	}
}
