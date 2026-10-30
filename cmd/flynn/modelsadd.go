package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/internal/huggingface"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
	"github.com/ionalpha/flynn/netguard"
)

// localRegistry is the [catalog.Source] Registry of an entry made from a file on this
// machine, which [userEntrySource] classifies as a local file whatever else the entry says.
const localRegistry = "local"

// unspecifiedLicense is recorded when a source declares none. A user's own entry may carry it
// where a curated entry may not, and the listing shows it as written.
const unspecifiedLicense = "unspecified"

// ollamaRegistry is the registry `flynn models add ollama:` reads manifests and blobs from.
// Tests point it at a local server.
var ollamaRegistry = "https://registry.ollama.com"

// ollamaHTTPClient is the client for the registry: public addresses only.
var ollamaHTTPClient = func() *http.Client { return netguard.Client(netguard.PublicOnly()) }

// addOptions are the flags every form of `flynn models add` accepts.
type addOptions struct {
	id, template, license, quant, file string
}

// takeAddOptions removes the add flags from args and returns the reference that is left.
func takeAddOptions(args []string) (addOptions, string, error) {
	var o addOptions
	args, o.id = takeValue(args, "--id")
	args, o.template = takeValueOr(args, "--chat-template", "chatml")
	args, o.license = takeValue(args, "--license")
	args, o.quant = takeValue(args, "--quant")
	args, o.file = takeValue(args, "--file")
	if len(args) != 1 || args[0] == "" {
		return o, "", errors.New("models add: give exactly one source: hf:owner/repo, ollama:name[:tag], or the path to a .gguf file")
	}
	if strings.Contains(o.id, ":") {
		return o, "", fmt.Errorf("models add: --id %q must not contain a colon: provider:model is how --model names a model on a running server", o.id)
	}
	return o, args[0], nil
}

// userSpec is the entry every form of add starts from. It is held at the local trust tier, and
// the capabilities are the least a goal needs; `flynn models probe` measures the rest.
func userSpec(id, name string, paramsB float64, src catalog.Source, license, template string, q catalog.Quant) catalog.ModelSpec {
	if license == "" {
		license = unspecifiedLicense
	}
	return catalog.ModelSpec{
		ID:           id,
		Name:         name,
		Kind:         catalog.KindLocal,
		Source:       src,
		License:      license,
		ParamsB:      paramsB,
		Capabilities: catalog.Capabilities{Tools: true},
		Quants:       []catalog.Quant{q},
		Trust:        catalog.TrustLocal,
		ChatTemplate: template,
	}
}

// finishAdd refuses an id the embedded catalog holds before asking for anything, shows the
// risk of the source the entry will be classified by, requires consent to it, and records the
// entry in the overlay. Consent is asked once, here, because the goal path serves a user entry
// without asking again.
func finishAdd(dataDir string, spec catalog.ModelSpec, autoApprove bool, out io.Writer) error {
	base, err := catalog.Load()
	if err != nil {
		return err
	}
	if _, refused := catalog.Merge(base, []catalog.ModelSpec{spec}); len(refused) > 0 {
		return fmt.Errorf("models add: %w", refused[0])
	}
	if err := confirmAdd(out, userEntrySource(spec), autoApprove); err != nil {
		return fmt.Errorf("models add: %w", err)
	}
	return recordAdd(dataDir, spec, out)
}

// recordAdd writes a consented entry to the overlay and says what to do next.
func recordAdd(dataDir string, spec catalog.ModelSpec, out io.Writer) error {
	replaced, err := catalog.AddUser(dataDir, spec)
	if err != nil {
		return fmt.Errorf("models add: %w", err)
	}
	verb := "added"
	if replaced {
		verb = "updated"
	}
	q := spec.Quants[0]
	_, _ = fmt.Fprintf(out, "%s %s (%s, %s, license %s) in %s\n", verb, spec.ID, q.Name, humanBytes(q.SizeBytes), spec.License, catalog.OverlayPath(dataDir))
	_, _ = fmt.Fprintln(out, "it is listed as unverified: no one has vetted it, and it is run only where its source can be contained.")
	_, _ = fmt.Fprintf(out, "next: `flynn models fetch %s`, then `flynn models probe %s` to measure it.\n", spec.ID, spec.ID)
	return nil
}

// selectGGUF chooses the single-file GGUF to add from a repository's files. A file or quant the
// user named must match; otherwise Q4_K_M is preferred, then the smallest. Projector files and
// shards are never candidates, and a file the Hub gives no content digest for is refused, since
// the entry would have nothing to verify the download against.
func selectGGUF(files []huggingface.File, file, quant string) (huggingface.File, error) {
	var cands []huggingface.File
	for _, f := range files {
		base := strings.ToLower(filepath.Base(f.Path))
		if !strings.HasSuffix(base, ".gguf") || strings.Contains(base, "mmproj") || shardedGGUF.MatchString(base) {
			continue
		}
		if file != "" && f.Path != file && filepath.Base(f.Path) != file {
			continue
		}
		if quant != "" && !strings.EqualFold(ggufQuantName(f.Path), quant) {
			continue
		}
		cands = append(cands, f)
	}
	if len(cands) == 0 {
		return huggingface.File{}, errors.New("no single-file GGUF matches (sharded models and projector files are not added; name one with --file or --quant)")
	}
	best := cands[0]
	for _, f := range cands[1:] {
		switch {
		case ggufQuantName(f.Path) == "Q4_K_M" && ggufQuantName(best.Path) != "Q4_K_M":
			best = f
		case (ggufQuantName(f.Path) == "Q4_K_M") == (ggufQuantName(best.Path) == "Q4_K_M") && f.Size < best.Size:
			best = f
		}
	}
	if best.SHA256 == "" {
		return huggingface.File{}, fmt.Errorf("%s is not stored in LFS, so the Hub records no content digest to verify it against", best.Path)
	}
	return best, nil
}

// addHub adds `hf:owner/repo[/file]`: the GGUF file, its size and its content digest are read
// from the Hub, so the entry pins the bytes the user was shown.
func addHub(ctx context.Context, hub *huggingface.Client, dataDir string, args []string, autoApprove bool, out io.Writer) error {
	o, ref, err := takeAddOptions(args)
	if err != nil {
		return err
	}
	src, err := modelsource.Parse(ref, nil)
	if err != nil || src.Kind != modelsource.KindHuggingFace {
		return fmt.Errorf("models add: %q is not a Hugging Face reference (use hf:owner/repo)", ref)
	}
	repoPath := src.Owner + "/" + src.Repo
	if o.file == "" {
		o.file = src.File
	}
	info, err := hub.Info(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("models add: read model card: %w", err)
	}
	if info.Gated {
		return fmt.Errorf("models add: %s is gated (it needs terms accepted on the Hub), so it cannot be fetched unattended", repoPath)
	}
	files, err := hub.Tree(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("models add: list files: %w", err)
	}
	f, err := selectGGUF(files, o.file, o.quant)
	if err != nil {
		return fmt.Errorf("models add: %s: %w", repoPath, err)
	}
	license := o.license
	if license == "" {
		license = info.License
	}
	id := o.id
	if id == "" {
		id = "hub:" + strings.ToLower(src.Repo)
	}
	spec := userSpec(id, src.Repo, paramsFromName(src.Repo),
		catalog.Source{Publisher: src.Owner, URL: "https://huggingface.co/" + repoPath, Registry: "huggingface"},
		license, o.template,
		catalog.Quant{Name: ggufQuantName(f.Path), Format: catalog.FormatGGUF, SizeBytes: f.Size, Ref: repoPath, URL: hub.FileURL(repoPath, f.Path), Digest: "sha256:" + f.SHA256})
	return finishAdd(dataDir, spec, autoApprove, out)
}

// ollamaNameRe is a registry path segment: lower case, so a reference cannot carry a path
// separator, a query or an upper-case alias of another name into the URL it builds.
var ollamaNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// parseOllamaRef splits ollama:[namespace/]name[:tag]. The namespace defaults to library and
// the tag to latest.
func parseOllamaRef(ref string) (ns, name, tag string, err error) {
	rest := strings.TrimPrefix(strings.TrimSpace(ref), "ollama:")
	rest, tag, hasTag := strings.Cut(rest, ":")
	if !hasTag {
		tag = "latest"
	}
	ns = "library"
	name = rest
	if n, nm, ok := strings.Cut(rest, "/"); ok {
		ns, name = n, nm
	}
	for _, seg := range []string{ns, name, tag} {
		if !ollamaNameRe.MatchString(seg) {
			return "", "", "", fmt.Errorf("%q is not an ollama reference (use ollama:name[:tag])", ref)
		}
	}
	return ns, name, tag, nil
}

// ollamaManifest is the part of a registry manifest the add reads.
type ollamaManifest struct {
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

const ollamaModelLayer = "application/vnd.ollama.image.model"

// addOllama adds `ollama:name[:tag]`: the registry manifest names the weights blob and its
// sha256, which the entry pins. The blob is fetched from the registry, never from the
// reference as the user typed it.
func addOllama(ctx context.Context, hc *http.Client, dataDir string, args []string, autoApprove bool, out io.Writer) error {
	o, ref, err := takeAddOptions(args)
	if err != nil {
		return err
	}
	ns, name, tag, err := parseOllamaRef(ref)
	if err != nil {
		return fmt.Errorf("models add: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v2/%s/%s/manifests/%s", ollamaRegistry, ns, name, tag), nil)
	if err != nil {
		return fmt.Errorf("models add: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("models add: read the ollama manifest: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("models add: the ollama registry has no %s/%s:%s (HTTP %d)", ns, name, tag, resp.StatusCode)
	}
	var man ollamaManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&man); err != nil {
		return fmt.Errorf("models add: decode the ollama manifest: %w", err)
	}
	var digest string
	var size int64
	for _, l := range man.Layers {
		if l.MediaType == ollamaModelLayer {
			digest, size = l.Digest, l.Size
		}
	}
	hexDigest, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hexDigest) != 64 || size <= 0 {
		return fmt.Errorf("models add: the manifest for %s/%s:%s names no weights blob with a sha256 digest", ns, name, tag)
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return fmt.Errorf("models add: the manifest for %s/%s:%s carries a malformed digest", ns, name, tag)
	}
	id := o.id
	if id == "" {
		id = "ollama.com/" + ns + "/" + name + ":" + tag
	}
	quant := o.quant
	if quant == "" {
		quant = tag
	}
	spec := userSpec(id, name, paramsFromName(tag+" "+name),
		catalog.Source{Publisher: ns, URL: fmt.Sprintf("%s/%s/%s", ollamaRegistry, ns, name), Registry: "ollama"},
		o.license, o.template,
		catalog.Quant{Name: quant, Format: catalog.FormatGGUF, SizeBytes: size, Ref: ns + "/" + name + ":" + tag, URL: fmt.Sprintf("%s/v2/%s/%s/blobs/%s", ollamaRegistry, ns, name, digest), Digest: digest})
	return finishAdd(dataDir, spec, autoApprove, out)
}

// addLocalFile adds a GGUF file on this machine: it is hashed, read once with the hardened
// reader, and copied into the data directory under the name the host looks for, so the host
// finds it already verified and never has to download it.
func addLocalFile(dataDir string, args []string, autoApprove bool, out io.Writer) error {
	o, ref, err := takeAddOptions(args)
	if err != nil {
		return err
	}
	if err := modelsource.CheckRunnableFormat(ref); err != nil {
		return fmt.Errorf("models add: %w", err)
	}
	if modelsource.DetectFormat(ref) != modelsource.FormatGGUF {
		return fmt.Errorf("models add: %s: only GGUF files can be added from disk", ref)
	}
	abs, err := filepath.Abs(ref)
	if err != nil {
		return fmt.Errorf("models add: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("models add: %s is not a file", ref)
	}
	stem := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	id := o.id
	if id == "" {
		id = "local:" + strings.ToLower(strings.NewReplacer(" ", "-").Replace(stem))
	}
	quant := o.quant
	if quant == "" {
		quant = ggufQuantName(filepath.Base(abs))
	}
	q := catalog.Quant{Name: quant, Format: catalog.FormatGGUF, SizeBytes: st.Size(), Ref: stem, URL: "file://" + filepath.ToSlash(abs)}
	spec := userSpec(id, stem, paramsFromName(stem),
		catalog.Source{Publisher: localRegistry, URL: "file://" + filepath.ToSlash(abs), Registry: localRegistry},
		o.license, o.template, q)

	// Check the id, and ask for consent, before copying a multi-gigabyte file.
	base, err := catalog.Load()
	if err != nil {
		return err
	}
	if _, refused := catalog.Merge(base, []catalog.ModelSpec{spec}); len(refused) > 0 {
		return fmt.Errorf("models add: %w", refused[0])
	}
	if err := confirmAdd(out, userEntrySource(spec), autoApprove); err != nil {
		return fmt.Errorf("models add: %w", err)
	}
	digest, err := copyIntoModels(dataDir, abs, weightsFileName(id, q))
	if err != nil {
		return fmt.Errorf("models add: %w", err)
	}
	spec.Quants[0].Digest = "sha256:" + digest
	_, _ = fmt.Fprintln(out, inspectWeights(filepath.Join(dataDir, "models", weightsFileName(id, q)), o.template))
	return recordAdd(dataDir, spec, out)
}

// copyIntoModels copies src to dataDir/models/name with [fsatomic.WriteStream], so a failed
// copy never leaves a partial weights file under the final name, and returns the sha256 of
// the bytes it wrote. The digest is taken from the copy,
// not from a separate read of the original, so it describes the file that will be served.
func copyIntoModels(dataDir, src, name string) (digest string, err error) {
	dir := filepath.Join(dataDir, "models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	in, err := os.Open(src) //nolint:gosec // the path the user asked to add
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	h := sha256.New()
	err = fsatomic.WriteStream(filepath.Join(dir, name), 0o600, func(w io.Writer) error {
		_, err := io.Copy(io.MultiWriter(w, h), in)
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
