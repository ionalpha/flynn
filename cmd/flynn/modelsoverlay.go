package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/huggingface"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
)

// catalogDataDir is the data directory whose catalog overlay [loadCatalog] merges. It is set
// once at startup from --data-dir; empty means no overlay, which is how the catalog behaved
// before the overlay existed.
var catalogDataDir string

// setCatalogDataDir points [loadCatalog] at a data directory's overlay.
func setCatalogDataDir(dir string) { catalogDataDir = dir }

// loadCatalog is the catalog every command reads: the embedded list with the user's overlay
// merged under it. An overlay that cannot be read, or an entry the merge refuses, never costs
// the embedded list; [loadCatalogNotes] says what was dropped.
func loadCatalog() (catalog.Catalog, error) {
	cat, _, err := loadCatalogNotes()
	return cat, err
}

// loadCatalogNotes is [loadCatalog] with a plain-language line for each thing the overlay
// lost: an unreadable file, or an entry refused by [catalog.Merge].
func loadCatalogNotes() (catalog.Catalog, []string, error) {
	cat, refused, err := catalog.LoadWithOverlay(catalogDataDir)
	var notes []string
	for _, r := range refused {
		notes = append(notes, "ignored your catalog entry "+r.Error())
	}
	if err != nil {
		// LoadWithOverlay returns the embedded list alongside an overlay error, so a nil
		// catalog here means the embedded list itself failed.
		if len(cat.Models) == 0 {
			return cat, nil, err
		}
		notes = append(notes, "ignored "+catalog.OverlayFile+": "+err.Error())
	}
	return cat, notes, nil
}

// isBlessedModelID reports whether spec names a local model in the embedded catalog. It is
// the only callback that may be given to [modelsource.Parse] as the catalog test, because a
// source parsed as a catalog entry is classified trusted and pinned: a user's own entry must
// never satisfy it, whatever it is called.
func isBlessedModelID(spec string) bool {
	cat, err := catalog.Load()
	if err != nil {
		return false
	}
	m, ok := findModel(cat, strings.TrimSpace(spec))
	return ok && m.Local()
}

// userModelEntry returns the overlay entry a model id names. An embedded id is never a user
// entry: [catalog.Merge] refuses an overlay entry that shares one.
func userModelEntry(id string) (catalog.ModelSpec, bool) {
	cat, err := loadCatalog()
	if err != nil {
		return catalog.ModelSpec{}, false
	}
	m, ok := findModel(cat, strings.TrimSpace(id))
	return m, ok && m.Local() && m.Unverified()
}

// userEntrySource is the source a user entry is classified by. It is read back from where the
// entry says its weights came from, the same way `flynn models run hf:owner/repo` would
// classify that reference, and never as a catalog entry. A claimed origin counts only when the
// weights are fetched from it: an entry that names a recognized publisher but downloads from
// elsewhere is classified as the direct URL it is.
func userEntrySource(m catalog.ModelSpec) modelsource.Source {
	if m.Source.Registry == localRegistry {
		return modelsource.Source{Kind: modelsource.KindFile, Raw: m.ID, Path: strings.TrimPrefix(m.Source.URL, "file://")}
	}
	src, err := modelsource.Parse(m.Source.URL, nil)
	if err != nil || src.Kind == modelsource.KindCatalog {
		return modelsource.Source{Kind: modelsource.KindFile, Raw: m.ID, Path: m.ID}
	}
	if src.Kind == modelsource.KindHuggingFace && !fetchedFromRepo(m, src) {
		return modelsource.Source{Kind: modelsource.KindURL, Raw: m.ID, URL: firstQuantURL(m)}
	}
	src.Raw = m.ID
	return src
}

// fetchedFromRepo reports whether every download an entry names is under the Hub repository
// it claims as its origin.
func fetchedFromRepo(m catalog.ModelSpec, src modelsource.Source) bool {
	prefix := "https://huggingface.co/" + src.Owner + "/" + src.Repo + "/"
	n := 0
	for _, q := range m.Quants {
		urls := []string{q.URL}
		for _, f := range q.Files {
			urls = append(urls, f.URL)
		}
		for _, u := range urls {
			if u == "" {
				continue
			}
			n++
			if !strings.HasPrefix(u, prefix) {
				return false
			}
		}
	}
	return n > 0
}

func firstQuantURL(m catalog.ModelSpec) string {
	for _, q := range m.Quants {
		if q.URL != "" {
			return q.URL
		}
		for _, f := range q.Files {
			if f.URL != "" {
				return f.URL
			}
		}
	}
	return ""
}

// parseModelRef parses a model reference for classification. A user's entry is classified by
// its origin ([userEntrySource]); only an embedded id parses as a catalog entry.
func parseModelRef(ref string) (modelsource.Source, error) {
	if m, ok := userModelEntry(ref); ok {
		return userEntrySource(m), nil
	}
	return modelsource.Parse(ref, isBlessedModelID)
}

// confirmAdd shows the risk of a source the user is about to add and requires consent to it,
// so the goal path, which asks for no consent at run time, only ever runs a user entry the
// person has already accepted.
func confirmAdd(out io.Writer, src modelsource.Source, autoApprove bool) error {
	class := modelsource.Classify(src, func(owner string) bool {
		return modelsource.KnownPublisher(owner, catalogPublishers()...)
	})
	rs := modelsource.DescribeRisk(src, class, modelsource.IntegrityUnverified)
	printRiskSurface(out, rs)
	return requireConsent(rs, stdinIsTerminal(), autoApprove, os.Stdin, out)
}

// runModelAdd implements `flynn models add <hf:owner/repo | ollama:name | path>`: record a
// model of the user's own in the catalog overlay, so it can be named by `--model`, `models
// probe` and `models fetch` like a catalog entry. The entry is held at the local trust tier
// and listed as unverified, and an id the embedded catalog holds is refused.
func runModelAdd(args []string, dataDir string, out io.Writer) error {
	autoApprove, args := takeFlag(args, "--yes", "-y")
	if len(args) == 0 || args[0] == "" {
		return errors.New("models add: a source is required: hf:owner/repo, ollama:name[:tag], or the path to a .gguf file")
	}
	setCatalogDataDir(dataDir)
	ctx := context.Background()
	ref := args[0]
	switch {
	case strings.HasPrefix(ref, "ollama:"):
		return addOllama(ctx, ollamaHTTPClient(), dataDir, args, autoApprove, out)
	case strings.HasPrefix(ref, "hf:") || strings.HasPrefix(ref, "https://huggingface.co/"):
		return addHub(ctx, huggingface.New(), dataDir, args, autoApprove, out)
	default:
		return addLocalFile(dataDir, args, autoApprove, out)
	}
}

// runModelRemove implements `flynn models remove <id>`: delete an entry the user added, and
// the weights that were downloaded or copied for it. A catalog entry is refused.
func runModelRemove(args []string, dataDir string, out io.Writer) error {
	if len(args) == 0 || args[0] == "" {
		return errors.New("models remove: a model id is required (see `flynn models`)")
	}
	id := args[0]
	user, err := catalog.ReadOverlay(dataDir)
	if err != nil {
		return fmt.Errorf("models remove: %w", err)
	}
	var entry catalog.ModelSpec
	for _, u := range user {
		if strings.EqualFold(strings.TrimSpace(u.ID), strings.TrimSpace(id)) {
			entry = u
		}
	}
	if err := catalog.RemoveUser(dataDir, id); err != nil {
		if errors.Is(err, catalog.ErrNotUserEntry) {
			return fmt.Errorf("models remove: %q is not one of your entries (`flynn models add` creates them)", id)
		}
		return fmt.Errorf("models remove: %w", err)
	}
	removed := 0
	for _, q := range entry.Quants {
		if q.MultiFile() {
			continue
		}
		// The name is derived from the id and quant, so it can only address the files this
		// entry's own downloads and copies were written to.
		path := filepath.Join(dataDir, "models", weightsFileName(entry.ID, q))
		if os.Remove(path) == nil {
			removed++
		}
	}
	_, _ = fmt.Fprintf(out, "removed %s from your catalog (%d downloaded file(s) deleted)\n", entry.ID, removed)
	return nil
}
