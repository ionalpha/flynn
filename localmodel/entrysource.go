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

// entrySource is the source a model that is not [TrustBlessed] is classified by. It is read
// back from where the entry says its weights come from, the way the same reference typed by
// hand would be, and is never a catalog source: a catalog source is classified trusted and
// pinned, and an entry anyone but the curator wrote has earned neither.
//
// A claimed origin counts only when the weights are fetched from it. An entry that names a
// recognized publisher but downloads from elsewhere is classified as the direct URL it is,
// so the publisher cannot be borrowed by a download link that points somewhere else.
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

// fetchedFromRepo reports whether every download an entry names is under the Hub repository
// it claims as its origin. An entry that names no download has no evidence of a fetch, so
// it does not count. The path is cleaned before it is compared, because a prefix test on the
// text would accept "/acme/tiny/../other/x.gguf" as a file of acme/tiny while the Hub serves
// it from other.
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
