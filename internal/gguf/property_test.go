package gguf

import (
	"bytes"
	"testing"

	"pgregory.net/rapid"
)

// TestProp_MetadataRoundTripsAndTemplateOverridden is the generative property behind
// the reader and the override: for any set of string metadata entries a model carries,
// including any chat template it embeds, every entry reads back exactly, and the chosen
// chat template is always the caller's trusted one while the model's attempt is
// reported. Whatever a model puts in its metadata, it cannot corrupt a read field or
// set the prompt contract.
func TestProp_MetadataRoundTripsAndTemplateOverridden(t *testing.T) {
	const trusted = "TRUSTED-CONTRACT"
	rapid.Check(t, func(rt *rapid.T) {
		// Keys are short and cannot collide with the chat-template key (which is
		// longer than this bound), so a generated entry never shadows the template.
		keyGen := rapid.StringMatching(`[a-z][a-z0-9_.]{0,20}`)
		keys := rapid.SliceOfDistinct(keyGen, func(s string) string { return s }).Draw(rt, "keys")

		want := make(map[string]string, len(keys)+1)
		kvs := make([]ggufKV, 0, len(keys)+1)
		for _, k := range keys {
			v := rapid.String().Draw(rt, "val")
			want[k] = v
			kvs = append(kvs, ggufKV{key: k, typ: typeString, val: v})
		}

		modelEmbedsTemplate := rapid.Bool().Draw(rt, "embedsTemplate")
		if modelEmbedsTemplate {
			tmpl := rapid.String().Draw(rt, "modelTemplate")
			want[chatTemplateKW] = tmpl
			kvs = append(kvs, ggufKV{key: chatTemplateKW, typ: typeString, val: tmpl})
		}

		meta, err := ReadMetadata(bytes.NewReader(buildGGUF(3, 0, kvs)))
		if err != nil {
			rt.Fatalf("read: %v", err)
		}
		for k, v := range want {
			got, ok := meta.String(k)
			if !ok || got != v {
				rt.Fatalf("key %q read back as %q (ok=%v), want %q", k, got, ok, v)
			}
		}

		dec := ChooseChatTemplate(meta, trusted)
		if dec.Template != trusted {
			rt.Fatalf("chosen template %q is not the trusted one", dec.Template)
		}
		if dec.ModelSupplied != modelEmbedsTemplate {
			rt.Fatalf("ModelSupplied=%v, want %v", dec.ModelSupplied, modelEmbedsTemplate)
		}
	})
}
