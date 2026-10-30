package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/huggingface"
	"github.com/ionalpha/flynn/internal/inference/launch"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
)

// runModelBless implements `flynn models bless <hf:owner/repo>`: resolve a Hugging Face
// model into a verified, digest-pinned catalog entry and save it to the user overlay. It
// turns an upstream reference into an entry without anyone hashing files by hand: the file
// manifest and every large file's content digest are read from the Hub, the small metadata
// files are fetched and hashed through the verified download path, and the result is
// assembled into a `catalog.ModelSpec`. The shipped catalog stays curated on purpose, so
// the entry goes to the overlay and is listed as unverified; a maintainer who reviews it
// can copy it from there into the shipped catalog.
//
// The trust anchor is the registry's own content addressing, captured here, not a
// hand-typed digest. A large weights file is pinned to the sha256 the Hub records as its
// LFS object id; a small file with no such id is downloaded once over the hardened,
// size-capped transport and pinned to the sha256 computed from the bytes.
func runModelBless(args []string, _ string, out io.Writer) error {
	return modelBless(context.Background(), huggingface.New(), fetch.New(), args, out)
}

// modelBless is the body of `flynn models bless` with the Hub client and the verified
// downloader supplied, so the reference parsing, the file selection, the refusals, and the
// printed entry are exercised against a stub Hub rather than the live one.
func modelBless(ctx context.Context, hub *huggingface.Client, dl *fetch.Downloader, args []string, out io.Writer) error {
	var idOverride, chatTemplate, licenseOverride, quantName string
	args, idOverride = takeValue(args, "--id")
	args, chatTemplate = takeValue(args, "--chat-template")
	args, licenseOverride = takeValue(args, "--license")
	args, quantName = takeValue(args, "--quant")

	if len(args) == 0 || args[0] == "" {
		return errors.New("models bless: a model reference is required, for example `flynn models bless hf:Qwen/Qwen2.5-7B-Instruct-AWQ`")
	}

	owner, repo, err := parseHubRef(args[0])
	if err != nil {
		return fmt.Errorf("models bless: %w", err)
	}
	repoPath := owner + "/" + repo
	if chatTemplate != "" && !launch.KnownChatTemplate(chatTemplate) {
		return fmt.Errorf("models bless: --chat-template %q is not a trusted template (one of %s)", chatTemplate, strings.Join(launch.TrustedChatTemplates(), ", "))
	}

	info, err := hub.Info(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("models bless: read model card: %w", err)
	}
	if info.Gated {
		return fmt.Errorf("models bless: %s is gated (requires accepting terms on the Hub), so it cannot be fetched and verified unattended; bless a non-gated mirror or accept the terms first", repoPath)
	}

	files, err := hub.Tree(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("models bless: list files: %w", err)
	}

	req := blessRequest{
		hub: hub, owner: owner, repo: repo, info: info, files: files,
		idOverride: idOverride, chatTemplate: chatTemplate,
		licenseOverride: licenseOverride, quantName: quantName,
	}
	if hasGGUF(files) {
		return blessGGUF(ctx, req, out)
	}
	return blessSafetensors(ctx, req, dl, out)
}

// resolveLicense returns the license the entry records: the model card's, or the one the
// caller confirmed with --license. A model with neither is refused, because the catalog
// does not ship a model whose terms nobody read.
func resolveLicense(req blessRequest) (string, error) {
	license := req.info.License
	if req.licenseOverride != "" {
		license = req.licenseOverride
	}
	if license == "" {
		return "", fmt.Errorf("models bless: %s/%s declares no license on its model card; pass --license <spdx-id> after confirming it", req.owner, req.repo)
	}
	return license, nil
}

// blessSafetensors builds the catalog entry for a repository that publishes safetensors
// weights, the format the vLLM runtime loads. Every file the runtime needs is pinned to a
// digest, and the model's own tokenizer files supply the chat template and reasoning
// setting, which are matched against the trusted set and never run.
func blessSafetensors(ctx context.Context, req blessRequest, dl *fetch.Downloader, out io.Writer) error {
	repoPath := req.owner + "/" + req.repo
	selected, format, err := selectServeFiles(req.files)
	if err != nil {
		return fmt.Errorf("models bless: %s: %w", repoPath, err)
	}

	// Resolve every selected file to a verified QuantFile. A large LFS file already
	// carries its content digest; a small file is downloaded once and hashed.
	tmpDir, err := os.MkdirTemp("", "flynn-bless-*")
	if err != nil {
		return fmt.Errorf("models bless: scratch dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	rs := &smallFileResolver{ctx: ctx, hub: req.hub, dl: dl, repoPath: repoPath, tmpDir: tmpDir, out: out}

	// The weight index names the shards that make up the model. Reading it first drops
	// a stray shard the repository also lists, which would otherwise be counted and
	// downloaded as weights nothing loads.
	if idx, ok := findFile(selected, weightIndexName); ok {
		_, data, err := rs.resolve(idx)
		if err != nil {
			return fmt.Errorf("models bless: %w", err)
		}
		if selected, err = pruneToWeightIndex(selected, data); err != nil {
			return fmt.Errorf("models bless: %s: %w", repoPath, err)
		}
	}

	quantFiles := make([]catalog.QuantFile, 0, len(selected))
	var totalBytes int64
	for _, f := range selected {
		qf, _, err := rs.resolve(f)
		if err != nil {
			return fmt.Errorf("models bless: %w", err)
		}
		totalBytes += qf.SizeBytes
		quantFiles = append(quantFiles, qf)
	}

	chatTemplate := req.chatTemplate
	src := rs.templateSource()
	if chatTemplate == "" {
		if name, ok := launch.MatchTemplate(src); ok {
			chatTemplate = name
		} else {
			chatTemplate = "chatml"
			_, _ = fmt.Fprintln(out, "no chat template found in the repository that matches a trusted one; defaulting to chatml, pass --chat-template to choose another")
		}
	}
	reasoning := launch.TemplateReasoning(src)

	cfg := parseModelConfig(rs.data("config.json"))
	quantName := req.quantName
	if quantName == "" {
		quantName = cfg.quantName()
	}
	license, err := resolveLicense(req)
	if err != nil {
		return err
	}

	id := req.idOverride
	if id == "" {
		id = "vllm:" + strings.ToLower(req.repo)
	}

	spec := catalog.ModelSpec{
		ID:   id,
		Name: req.repo,
		Kind: catalog.KindLocal,
		Source: catalog.Source{
			Publisher: req.owner,
			URL:       "https://huggingface.co/" + repoPath,
			Registry:  "huggingface",
		},
		License:       license,
		ParamsB:       paramsFromName(req.repo),
		ContextTokens: cfg.ContextTokens,
		Capabilities:  catalog.Capabilities{Tools: launch.NativeToolCalls(chatTemplate), Reasoning: reasoning},
		Quants: []catalog.Quant{{
			Name:      quantName,
			Format:    format,
			SizeBytes: totalBytes,
			Ref:       repoPath,
			Files:     quantFiles,
		}},
		Trust:        catalog.TrustBlessed,
		ChatTemplate: chatTemplate,
	}

	_, _ = fmt.Fprintf(out, "\nresolved %s: %d files, %s, format %s, quant %s, context %d, chat template %s, reasoning %v\n",
		repoPath, len(quantFiles), humanBytes(totalBytes), format, quantName, cfg.ContextTokens, chatTemplate, reasoning)
	return saveBlessedEntry(catalogDataDir, spec, out)
}

// saveBlessedEntry stores a blessed entry in the user overlay under dataDir, where it is
// listed as unverified until a maintainer reviews it into the shipped catalog. An id the
// shipped catalog already owns is refused before anything is written, because the overlay
// could not list it anyway. With no data directory there is nowhere to save, so the entry
// is printed as JSON for a maintainer to commit by hand.
func saveBlessedEntry(dataDir string, spec catalog.ModelSpec, out io.Writer) error {
	if dataDir == "" {
		_, _ = fmt.Fprintf(out, "run `flynn models probe %s` after committing to measure real agent reliability\n", spec.ID)
		_, _ = fmt.Fprintln(out, "review and add this entry to the curated catalog (catalog/models.json):")
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(spec); err != nil {
			return fmt.Errorf("models bless: print entry: %w", err)
		}
		return nil
	}
	if err := catalog.CheckUser(spec); err != nil {
		return fmt.Errorf("models bless: %w", err)
	}
	replaced, err := catalog.AddUser(dataDir, spec)
	if err != nil {
		return fmt.Errorf("models bless: %w", err)
	}
	verb := "saved"
	if replaced {
		verb = "updated"
	}
	_, _ = fmt.Fprintf(out, "%s %s in %s (listed as unverified)\n", verb, spec.ID, catalog.OverlayPath(dataDir))
	_, _ = fmt.Fprintf(out, "run `flynn models probe %s` to measure real agent reliability\n", spec.ID)
	return nil
}

// selectServeFiles keeps the files a runtime loads a model from (the weights, the
// tokenizer, the config and the chat template) at the repository root and drops the
// repository's documentation, media and subdirectories. It refuses a repo whose only weights are a code-executing format, so an unsafe model is
// never blessed; when a safetensors set is present, any stray unsafe file is simply left
// out. It returns the kept files and the weight format they make up.
func selectServeFiles(files []huggingface.File) ([]huggingface.File, catalog.Format, error) {
	var safetensors, pickle bool
	kept := make([]huggingface.File, 0, len(files))
	for _, f := range files {
		// A runtime loads a model from the repository root. A subdirectory holds
		// another packaging of the same weights (original/, metal/), and carrying it
		// would count and download them twice.
		if strings.Contains(f.Path, "/") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Path))
		switch ext {
		case ".safetensors":
			safetensors = true
		case ".bin", ".pt", ".pth", ".ckpt":
			pickle = true
			continue // never carry a pickle weight into the manifest
		}
		if servingFile(f.Path) {
			kept = append(kept, f)
		}
	}
	if !safetensors {
		if pickle {
			return nil, "", errors.New("only code-executing weight files (pickle) are published; refusing to bless an unsafe format")
		}
		return nil, "", errors.New("no safetensors weights found to serve")
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Path < kept[j].Path })
	return kept, catalog.FormatSafetensors, nil
}

// servingFile reports whether a file is part of what a runtime loads (weights,
// tokenizer, config) rather than repository documentation or media. It keeps the
// safetensors shards, the weight index, and the JSON/text tokenizer and config files,
// and drops licenses, readmes, images, and git metadata.
func servingFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".safetensors":
		return true
	case ".json", ".txt", ".model":
		// Keep tokenizer/config text, drop documentation text files.
		if base == "readme.txt" {
			return false
		}
		return true
	case ".jinja":
		// The chat template a tokenizer ships beside its config.
		return base == "chat_template.jinja"
	}
	return false
}

// modelConfig is the subset of a model's config.json that shapes a catalog entry.
type modelConfig struct {
	ContextTokens int
	QuantMethod   string
}

// parseModelConfig reads the fields used from config.json, tolerating a missing or
// unreadable config by returning zero values: a blank context or quant is a softer
// failure than refusing to bless, and a reviewer sees the gap in the printed entry.
func parseModelConfig(b []byte) modelConfig {
	if len(b) == 0 {
		return modelConfig{}
	}
	var raw struct {
		MaxPositionEmbeddings int `json:"max_position_embeddings"`
		QuantizationConfig    struct {
			QuantMethod string `json:"quant_method"`
		} `json:"quantization_config"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return modelConfig{}
	}
	return modelConfig{
		ContextTokens: raw.MaxPositionEmbeddings,
		QuantMethod:   strings.ToLower(raw.QuantizationConfig.QuantMethod),
	}
}

// quantName names the quantization for the catalog entry. It prefers the scheme the
// model's config declares (awq, gptq, fp8) so the served `--quantization` is unambiguous,
// and falls back to the full-precision label when no scheme is declared.
func (c modelConfig) quantName() string {
	switch c.QuantMethod {
	case "awq":
		return "awq"
	case "gptq":
		return "gptq"
	case "fp8":
		return "fp8"
	case "":
		return "fp16"
	default:
		return c.QuantMethod
	}
}

var paramsRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*b\b`)

// paramsFromName reads the parameter count out of a model name like "Qwen2.5-7B-Instruct"
// (7.0). It returns 0 when the name carries no size, which is recorded as undisclosed
// rather than guessed.
func paramsFromName(name string) float64 {
	m := paramsRe.FindStringSubmatch(name)
	if len(m) < 2 {
		return 0
	}
	var v float64
	if _, err := fmt.Sscanf(m[1], "%g", &v); err != nil {
		return 0
	}
	return v
}

// parseHubRef accepts the hub reference forms (hf:owner/repo, a huggingface.co URL, or a
// bare owner/repo) and returns the owner and repository name. A bare owner/repo is read
// as a hub reference here because the command only ever blesses hub models.
func parseHubRef(ref string) (owner, repo string, err error) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "hf:") && !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") {
		ref = "hf:" + ref
	}
	src, err := modelsource.Parse(ref, nil)
	if err != nil {
		return "", "", err
	}
	if src.Kind != modelsource.KindHuggingFace || src.Owner == "" || src.Repo == "" {
		return "", "", fmt.Errorf("not a Hugging Face model reference: %s (use hf:owner/repo)", ref)
	}
	return src.Owner, src.Repo, nil
}

// takeValue removes a `--flag value` pair from args and returns the value, or "" when the
// flag is absent. It pairs with takeFlag (which handles boolean flags) for the small,
// dependency-free flag parsing these subcommands use.
func takeValue(args []string, name string) (rest []string, value string) {
	out := args[:0:0]
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			value = args[i+1]
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out, value
}

// takeValueOr is takeValue with a default returned when the flag is absent.
func takeValueOr(args []string, name, def string) (rest []string, value string) {
	rest, v := takeValue(args, name)
	if v == "" {
		v = def
	}
	return rest, v
}
