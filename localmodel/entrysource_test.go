package localmodel_test

import (
	"testing"

	"github.com/ionalpha/flynn/localmodel"
)

// Each origin an entry can name is classified as the source it is, not merely as something
// short of the catalog: a file, a direct URL, a Hub repository the weights come from, and
// a Hub claim the weights do not come from.
func TestEntrySourceNamesTheOriginItClaims(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	const repo = "https://huggingface.co/acme/tiny/resolve/main/tiny.gguf"
	hub := localmodel.Source{Publisher: "acme", URL: "https://huggingface.co/acme/tiny"}
	cases := []struct {
		name       string
		id         string
		origin     localmodel.Source
		quant      string
		wantKind   string
		wantTarget string
	}{
		{"local file", "mine:1b", localmodel.Source{Publisher: "local", Registry: "local", URL: "file:///tmp/x.gguf"}, repo, "file", "/tmp/x.gguf"},
		{"direct URL", "mine:1b", localmodel.Source{Publisher: "acme", URL: "https://example.test/x.gguf"}, "https://example.test/x.gguf", "url", "https://example.test/x.gguf"},
		{"hub repository the weights come from", "mine:1b", hub, repo, "hub", "acme/tiny"},
		{"hub claim fetched from elsewhere", "mine:1b", hub, "https://example.test/x.gguf", "url", "https://example.test/x.gguf"},
		{"hub claim fetched through a parent segment", "mine:1b", hub, "https://huggingface.co/acme/tiny/../other/resolve/main/x.gguf", "url", "https://huggingface.co/acme/tiny/../other/resolve/main/x.gguf"},
		{"hub claim fetched through an encoded parent segment", "mine:1b", hub, "https://huggingface.co/acme/tiny/%2e%2e/other/x.gguf", "url", "https://huggingface.co/acme/tiny/%2e%2e/other/x.gguf"},
		{"hub claim fetched over plain http", "mine:1b", hub, "http://huggingface.co/acme/tiny/resolve/main/x.gguf", "url", "http://huggingface.co/acme/tiny/resolve/main/x.gguf"},
		{"hub claim fetched from a lookalike host", "mine:1b", hub, "https://huggingface.co.example.test/acme/tiny/x.gguf", "url", "https://huggingface.co.example.test/acme/tiny/x.gguf"},
		{"unparseable origin", "mine-1b.gguf", localmodel.Source{Publisher: "acme", URL: "hf:broken"}, "https://example.test/x.gguf", "file", "mine-1b.gguf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := entryAt(f, tc.id, localmodel.TrustLocal, tc.origin)
			m.Quants[0].URL = tc.quant
			kind, target := localmodel.EntrySource(m)
			if kind != tc.wantKind || target != tc.wantTarget {
				t.Fatalf("EntrySource = %s %q; want %s %q", kind, target, tc.wantKind, tc.wantTarget)
			}
		})
	}
}
