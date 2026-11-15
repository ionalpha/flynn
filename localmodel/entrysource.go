package localmodel

import (
	"net/url"
	"path"
	"strings"

	"github.com/ionalpha/flynn/internal/inference/modelsource"
)

// localRegistry is the [Source] Registry of an entry made from a weights file already on
// this machine. The flynn binary's `models add` writes it, so a host that merges the same
// overlay gets entries this file can read back.
const localRegistry = "local"

// entrySource classifies a model that is not [TrustBlessed] by where its weights come
// from, as the same reference typed by hand would be, and never as a catalog source,
// because only the curator's entries earn pinned trust. A claimed origin counts only when
// the weights are fetched from it, so a publisher's name cannot be borrowed by a download
// link that points elsewhere.
func entrySource(m Model) modelsource.Source {
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

// fetchedFromRepo reports whether every download an entry names is under the Hub
// repository it claims. An entry with no download does not count. Paths are cleaned
// first, because a text prefix test would accept "/acme/tiny/../other/x.gguf" as a file
// of acme/tiny.
func fetchedFromRepo(m Model, src modelsource.Source) bool {
	prefix := "/" + src.Owner + "/" + src.Repo + "/"
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
			if !inRepo(u, prefix) {
				return false
			}
		}
	}
	return n > 0
}

// inRepo reports whether u is an https URL on the Hub whose cleaned path is under prefix.
func inRepo(u, prefix string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.Host != "huggingface.co" || p.User != nil {
		return false
	}
	return strings.HasPrefix(path.Clean("/"+p.Path), prefix)
}

// firstQuantURL is the first download an entry names, or empty when it names none.
func firstQuantURL(m Model) string {
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
