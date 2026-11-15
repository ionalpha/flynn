// Package modelsource classifies where a model's weights come from and how far that
// source can be trusted, so every model (a catalog entry, a hub reference, a raw URL or
// a dropped file) passes one trust decision before it is fetched or run. A model file
// is untrusted input to a parser with a history of memory-safety flaws, so its source
// sets the containment a run requires: the pinned catalog is the trust anchor, a
// recognized publisher is semi-trusted, and anything else is untrusted and runs only
// where it can be contained. The package does no I/O; fetching, pinning and running
// are built on top of it.
package modelsource

import (
	"path"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/sandbox"
)

// Kind is where a model's weights come from. It decides how the reference resolves to
// bytes; trust also depends on the publisher.
type Kind int

const (
	// KindCatalog is an entry in the embedded catalog: pinned URL and digest, vetted
	// offline, so trust does not depend on any live registry.
	KindCatalog Kind = iota
	// KindHuggingFace is a model-hub reference, hf:owner/repo[/file] or a huggingface.co
	// URL. The owner is the publisher the trust decision turns on.
	KindHuggingFace
	// KindURL is a direct https link to a weights file from no recognized publisher.
	KindURL
	// KindFile is a local weights file a user points at or drops in.
	KindFile
)

// Source is a parsed, not-yet-fetched model reference. Only the fields for its Kind
// are set.
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

// Parse classifies a reference string into a Source. isCatalogID is consulted first, so
// a catalog id is never read as a path. The grammar has four unambiguous forms: a
// catalog id, an hf: prefix or huggingface.co URL, any other https URL, or a local path.
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
	// Anything else is a local path.
	return Source{Kind: KindFile, Raw: ref, Path: ref}, nil
}

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
	// A resolve URL embeds the file after /resolve/<rev>/; the file is kept and the
	// revision dropped so owner/repo stay clean.
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

// Key identifies the source for provenance and for pinning its integrity on first use.
// Two references that resolve to the same weights share a key.
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
	// FormatUnknown is an unrecognized extension. It is refused, because an unknown
	// container cannot be assumed safe to parse.
	FormatUnknown WeightFormat = iota
	// FormatGGUF is the single-file quantized format, read by the hardened reader.
	FormatGGUF
	// FormatSafetensors stores tensors only and executes no code on load.
	FormatSafetensors
	// FormatCodeExecuting can run arbitrary code when loaded (a Python pickle, or an
	// archive that could carry one). It is refused everywhere.
	FormatCodeExecuting
)

// DetectFormat names the weight format from a filename's extension. An empty or
// extension-less name is unknown.
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

// CheckRunnableFormat allows only GGUF and safetensors: a code-executing format is
// refused, and so is an unrecognized one. It applies to every source, so a dropped
// pickle file is refused the same as a downloaded one.
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

// Classification is the trust decision for a source, with a reason a user can be shown.
type Classification struct {
	// Trust sets the containment a run requires through sandbox.Required.
	Trust sandbox.Trust
	// Reason explains in plain language why the source got this trust.
	Reason string
}

// Classify maps a source to a trust level. A catalog entry is trusted. A hub model from
// a publisher knownPublisher recognizes is semi-trusted and needs kernel confinement,
// because its bytes are still parsed by a vulnerable runtime. Anything else is untrusted.
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
