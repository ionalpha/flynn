package catalog_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/inference/launch"
)

var sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// TestShippedLocalModelsAreFetchableAndServable guards the entries a user can pull with no
// further input. A direct GGUF is fetched from its url and checked against its digest, so
// both must be present and the url must name the file in ref. A chat template the launcher
// does not accept would make the model fail only when it is started, long after the
// catalog was merged.
func TestShippedLocalModelsAreFetchableAndServable(t *testing.T) {
	c, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	var moe bool
	for _, m := range c.Models {
		if m.Kind != catalog.KindLocal {
			continue
		}
		if !launch.KnownChatTemplate(m.ChatTemplate) {
			t.Errorf("%s: chatTemplate %q is not one the launcher accepts", m.ID, m.ChatTemplate)
		}
		if strings.Contains(strings.ToLower(m.Name), "moe") {
			moe = true
		}
		for _, q := range m.Quants {
			if q.Format != catalog.FormatGGUF || q.URL == "" {
				continue
			}
			if !sha256Digest.MatchString(q.Digest) {
				t.Errorf("%s %s: digest %q is not sha256:<64 hex>", m.ID, q.Name, q.Digest)
			}
			if !strings.HasSuffix(q.URL, "/"+q.Ref) {
				t.Errorf("%s %s: url %q does not end in ref %q", m.ID, q.Name, q.URL, q.Ref)
			}
			if q.SizeBytes <= 0 {
				t.Errorf("%s %s: sizeBytes must be positive", m.ID, q.Name)
			}
		}
	}
	if !moe {
		t.Error("the shipped catalog should carry at least one mixture-of-experts local model")
	}
}
