package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/gguf"
	"github.com/ionalpha/flynn/internal/huggingface"
	"github.com/ionalpha/flynn/internal/inference/launch"
	"github.com/ionalpha/flynn/netguard"
)

// ggufHeaderLimit caps how much of a GGUF file bless reads to find its metadata. The
// header holds the vocabulary and the chat template, a few megabytes in practice; the
// tensors that follow are tens of gigabytes and are never read.
const ggufHeaderLimit = 64 << 20

// ggufHeaderClient reads the start of a GGUF file. It is the same hardened client the
// downloader uses (public addresses only, https only, redirects re-checked), and a
// variable so a test can point it at a local server.
var ggufHeaderClient = netguard.Client(netguard.PublicOnly())

// blessRequest carries what blessGGUF needs from the flags and the Hub listing.
type blessRequest struct {
	hub             *huggingface.Client
	owner, repo     string
	info            huggingface.Info
	files           []huggingface.File
	idOverride      string
	chatTemplate    string // a trusted name from --chat-template, or "" to detect
	licenseOverride string
	quantName       string // --quant, or "" for every quant the repository publishes
}

func hasGGUF(files []huggingface.File) bool {
	for _, f := range files {
		if isGGUFFile(f.Path) {
			return true
		}
	}
	return false
}

func isGGUFFile(path string) bool { return strings.HasSuffix(strings.ToLower(path), ".gguf") }

// skippedFile is a repository file bless leaves out of the entry, with the reason it
// prints so the omission is visible to a reviewer.
type skippedFile struct{ path, reason string }

var (
	// shardedGGUF matches the name llama.cpp's split tool gives a piece of a model.
	shardedGGUF = regexp.MustCompile(`(?i)-\d{5}-of-\d{5}\.gguf$`)
	// auxiliaryGGUF matches files that live beside a model and are not one: the vision
	// projector, a multi-token-prediction head, a speculative-decoding draft, and the
	// importance matrix used to make a quant.
	auxiliaryGGUF = regexp.MustCompile(`(?i)(^|[-_.])(mmproj|mtp|eagle3?|draft|imatrix)([-_.]|$)`)
	// ggufQuantLabel finds the quantization in a file name: a k-quant or i-quant (Q4_K_M,
	// IQ3_XS, q4_0), MXFP4, or an unquantized float type.
	ggufQuantLabel = regexp.MustCompile(`(?i)(?:^|[-_.])(I?Q\d(?:_[A-Z0-9]+)*|MXFP4(?:_MOE)?|BF16|F16|F32)(?:[-_.]|$)`)
)

// selectGGUFModels returns the GGUF files that are a whole, loadable model, ordered by
// name, and the GGUF files it leaves out. The catalog fetches one URL per quant, so a
// model split across files cannot be expressed and is skipped with a note.
func selectGGUFModels(files []huggingface.File) (models []huggingface.File, skipped []skippedFile) {
	for _, f := range files {
		if !isGGUFFile(f.Path) {
			continue
		}
		switch {
		case strings.Contains(f.Path, "/"):
			skipped = append(skipped, skippedFile{f.Path, "in a subdirectory"})
		case shardedGGUF.MatchString(f.Path):
			skipped = append(skipped, skippedFile{f.Path, "one piece of a split model; the catalog fetches a single file per quant"})
		case auxiliaryGGUF.MatchString(f.Path):
			skipped = append(skipped, skippedFile{f.Path, "not a standalone model (projector, draft or importance matrix)"})
		case f.SHA256 == "":
			skipped = append(skipped, skippedFile{f.Path, "the Hub lists no content digest for it"})
		default:
			models = append(models, f)
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Path < models[j].Path })
	return models, skipped
}

// ggufQuantName names the quantization of a GGUF file, falling back to the file name
// without its extension.
func ggufQuantName(file string) string {
	if m := ggufQuantLabel.FindStringSubmatch(file); m != nil {
		return strings.ToUpper(m[1])
	}
	return strings.TrimSuffix(file, file[len(file)-len(".gguf"):])
}

// readGGUFHeader streams the start of a GGUF file over the hardened client and parses
// its metadata. The body is closed as soon as the header is read, so the weights that
// follow are not downloaded.
func readGGUFHeader(ctx context.Context, url string) (*gguf.Metadata, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("refusing to read %s over a non-https URL", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := ggufHeaderClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return gguf.ReadMetadata(io.LimitReader(resp.Body, ggufHeaderLimit))
}

// filterQuant narrows the model files to the quant the caller asked for with --quant,
// matching either the quantization label or the file name. An empty name keeps them all.
func filterQuant(models []huggingface.File, quantName, repoPath string) ([]huggingface.File, error) {
	if quantName == "" {
		return models, nil
	}
	var chosen []huggingface.File
	names := make([]string, 0, len(models))
	for _, f := range models {
		names = append(names, ggufQuantName(f.Path))
		if strings.EqualFold(ggufQuantName(f.Path), quantName) || strings.EqualFold(f.Path, quantName) {
			chosen = append(chosen, f)
		}
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("models bless: %s has no quant %q (available: %s)", repoPath, quantName, strings.Join(names, ", "))
	}
	return chosen, nil
}

// chooseGGUFTemplate picks the trusted template name for a model. The requested name
// wins; otherwise the name is matched from the control tokens in the model's own
// template, which is only searched and never run. A model whose template matches nothing
// is refused, because guessing would serve it under a prompt format it was not trained on.
func chooseGGUFTemplate(requested, file, embedded string, out io.Writer) (string, error) {
	matched, found := launch.MatchTemplate(embedded)
	switch {
	case requested == "" && found:
		return matched, nil
	case requested == "":
		why := "carries no chat template"
		if embedded != "" {
			why = "carries a chat template that matches none of the trusted ones"
		}
		return "", fmt.Errorf("models bless: %s %s; pass --chat-template with one of %s once you have confirmed which fits", file, why, strings.Join(launch.TrustedChatTemplates(), ", "))
	case found && matched != requested:
		_, _ = fmt.Fprintf(out, "note: the model's own template looks like %s; using %s as requested\n", matched, requested)
	}
	return requested, nil
}

// blessGGUF builds the catalog entry for a repository that publishes GGUF files. The
// entry has one quant per model file, each pinned to the content digest the Hub records
// for it. The model's own metadata supplies the context length and the chat template
// text; the template is only searched for the control tokens that identify a trusted
// template, so the entry names a reviewed template and never carries the model's.
func blessGGUF(ctx context.Context, req blessRequest, out io.Writer) error {
	repoPath := req.owner + "/" + req.repo
	models, skipped := selectGGUFModels(req.files)
	for _, s := range skipped {
		_, _ = fmt.Fprintf(out, "skipping %s: %s\n", s.path, s.reason)
	}
	if len(models) == 0 {
		return fmt.Errorf("models bless: %s: no standalone GGUF model file to serve", repoPath)
	}
	models, err := filterQuant(models, req.quantName, repoPath)
	if err != nil {
		return err
	}

	license, err := resolveLicense(req)
	if err != nil {
		return err
	}

	// Every quant of a repository carries the same metadata, so the first file's header
	// stands for them all.
	meta, err := readGGUFHeader(ctx, req.hub.FileURL(repoPath, models[0].Path))
	if err != nil {
		return fmt.Errorf("models bless: read the GGUF header of %s: %w", models[0].Path, err)
	}
	embedded, _ := meta.ChatTemplate()

	chatTemplate, err := chooseGGUFTemplate(req.chatTemplate, models[0].Path, embedded, out)
	if err != nil {
		return err
	}
	reasoning := launch.TemplateReasoning(embedded)

	quants := make([]catalog.Quant, 0, len(models))
	for _, f := range models {
		quants = append(quants, catalog.Quant{
			Name:      ggufQuantName(f.Path),
			Format:    catalog.FormatGGUF,
			SizeBytes: f.Size,
			Ref:       f.Path,
			URL:       req.hub.FileURL(repoPath, f.Path),
			Digest:    "sha256:" + f.SHA256,
		})
	}

	name := ggufModelName(req.repo)
	id := req.idOverride
	if id == "" {
		id = strings.ToLower(name)
	}
	contextTokens := clampContext(meta.ContextLength())
	if contextTokens == 0 {
		_, _ = fmt.Fprintln(out, "the GGUF header does not declare a context length; contextTokens is left unset")
	}

	spec := catalog.ModelSpec{
		ID:   id,
		Name: name,
		Kind: catalog.KindLocal,
		Source: catalog.Source{
			Publisher: req.owner,
			URL:       "https://huggingface.co/" + repoPath,
			Registry:  "huggingface",
		},
		License:       license,
		ParamsB:       paramsFromName(req.repo),
		ContextTokens: contextTokens,
		Capabilities:  catalog.Capabilities{Tools: launch.NativeToolCalls(chatTemplate), Reasoning: reasoning},
		Quants:        quants,
		Trust:         catalog.TrustBlessed,
		ChatTemplate:  chatTemplate,
	}
	entry, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("models bless: encode entry: %w", err)
	}

	arch := meta.Architecture()
	if arch == "" {
		arch = "unknown"
	}
	_, _ = fmt.Fprintf(out, "\nresolved %s: %d GGUF quant(s), architecture %s, context %d, chat template %s, reasoning %v\n",
		repoPath, len(quants), arch, contextTokens, chatTemplate, reasoning)
	if !launch.NativeToolCalls(chatTemplate) {
		_, _ = fmt.Fprintf(out, "the %s template runs without Jinja, so the server cannot offer native tool calls; tools is false\n", chatTemplate)
	}
	_, _ = fmt.Fprintf(out, "run `flynn models probe %s` after committing to measure real agent reliability\n", id)
	_, _ = fmt.Fprintln(out, "review and add this entry to the curated catalog (catalog/models.json):")
	_, _ = fmt.Fprintln(out, string(entry))
	return nil
}

// clampContext converts a context length from a model header to an int. The header is
// untrusted, so a value no real model has is capped rather than allowed to wrap.
func clampContext(n uint64) int {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(n)
}

// ggufModelName is the repository name without the -GGUF suffix publishers add.
func ggufModelName(repo string) string {
	const suffix = "-gguf"
	if len(repo) > len(suffix) && strings.EqualFold(repo[len(repo)-len(suffix):], suffix) {
		return repo[:len(repo)-len(suffix)]
	}
	return repo
}
