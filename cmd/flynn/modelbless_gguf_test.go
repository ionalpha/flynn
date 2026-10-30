package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/huggingface"
)

// ggufHeader encodes the start of a GGUF file: the architecture, a context length when
// ctx is non-zero, and a chat template when tmpl is non-empty. It carries no tensors,
// which is all bless ever reads.
func ggufHeader(arch string, ctx uint32, tmpl string) []byte {
	var b bytes.Buffer
	w32 := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	w64 := func(v uint64) { _ = binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { w64(uint64(len(s))); b.WriteString(s) }
	n := uint64(1)
	if ctx != 0 {
		n++
	}
	if tmpl != "" {
		n++
	}
	w32(0x46554747)
	w32(3)
	w64(0)
	w64(n)
	str("general.architecture")
	w32(8)
	str(arch)
	if ctx != 0 {
		str(arch + ".context_length")
		w32(4)
		w32(ctx)
	}
	if tmpl != "" {
		str("tokenizer.chat_template")
		w32(8)
		str(tmpl)
	}
	return b.Bytes()
}

const (
	ossTemplate   = "<|start|>{{ role }}<|channel|>analysis<|message|>{{ x }} reasoning_effort"
	chatmlText    = "<|im_start|>{{ role }}\n{{ content }}<|im_end|>"
	gemma4Text    = "<|turn>user\n{{ content }}<turn|> <|think|>"
	llama3Text    = "<|start_header_id|>user<|end_header_id|>"
	unknownFormat = "{{ messages }}"
)

func ggufRepo(header []byte, extra ...map[string]any) blessRepo {
	sha := strings.Repeat("ab", 32)
	tree := []map[string]any{
		blessLFS("model-MXFP4.gguf", 12_000_000_000, sha),
		blessSmall("README.md", 100),
	}
	return blessRepo{
		info:  map[string]any{"id": "acme/tiny-20b-GGUF", "cardData": map[string]any{"license": "apache-2.0"}},
		tree:  append(tree, extra...),
		files: map[string][]byte{"model-MXFP4.gguf": header},
	}
}

func bless(t *testing.T, repo blessRepo, args ...string) (string, error) {
	t.Helper()
	hub, dl := newBlessHub(t, repo)
	var out bytes.Buffer
	err := modelBless(context.Background(), hub, dl, append(args, "acme/tiny-20b-GGUF"), &out)
	return out.String(), err
}

// TestModelBlessGGUFReadsTheModelsOwnMetadata is the headline: a GGUF repo yields one quant
// per model file, pinned to the Hub digest, with the context, template and reasoning read
// from the file header and the trusted template named rather than copied.
func TestModelBlessGGUFReadsTheModelsOwnMetadata(t *testing.T) {
	out, err := bless(t, ggufRepo(ggufHeader("gptoss", 131072, ossTemplate)))
	if err != nil {
		t.Fatalf("modelBless: %v\n%s", err, out)
	}
	spec := blessedSpec(t, out)
	if spec.ID != "tiny-20b" || spec.Name != "tiny-20b" {
		t.Errorf("id = %q name = %q, want the repository name without -GGUF", spec.ID, spec.Name)
	}
	if spec.ChatTemplate != "gpt-oss" {
		t.Errorf("chatTemplate = %q", spec.ChatTemplate)
	}
	if strings.Contains(out, "{{ x }}") {
		t.Error("the template text from the model must not be copied into the entry")
	}
	if spec.ContextTokens != 131072 || spec.ParamsB != 20 {
		t.Errorf("context = %d params = %v", spec.ContextTokens, spec.ParamsB)
	}
	if !spec.Capabilities.Tools || !spec.Capabilities.Reasoning {
		t.Errorf("capabilities = %+v, want tools and reasoning", spec.Capabilities)
	}
	if len(spec.Quants) != 1 {
		t.Fatalf("quants = %+v", spec.Quants)
	}
	q := spec.Quants[0]
	if q.Name != "MXFP4" || q.Format != catalog.FormatGGUF || q.Ref != "model-MXFP4.gguf" || q.SizeBytes != 12_000_000_000 {
		t.Errorf("quant = %+v", q)
	}
	if q.Digest != "sha256:"+strings.Repeat("ab", 32) || !strings.HasSuffix(q.URL, "/model-MXFP4.gguf") || len(q.Files) != 0 {
		t.Errorf("quant pin = %+v", q)
	}
	if err := (catalog.Catalog{Models: []catalog.ModelSpec{spec}}).Validate(); err != nil {
		t.Errorf("blessed entry does not validate: %v", err)
	}
}

func TestModelBlessGGUFPicksTheTemplateFromTheTokens(t *testing.T) {
	for _, c := range []struct {
		tmpl, want string
		tools      bool
		reasoning  bool
	}{
		{gemma4Text, "gemma4", true, true},
		{chatmlText, "chatml", true, false},
		{llama3Text, "llama3", false, false},
	} {
		out, err := bless(t, ggufRepo(ggufHeader("llama", 4096, c.tmpl)))
		if err != nil {
			t.Fatalf("%s: %v", c.want, err)
		}
		spec := blessedSpec(t, out)
		if spec.ChatTemplate != c.want || spec.Capabilities.Tools != c.tools || spec.Capabilities.Reasoning != c.reasoning {
			t.Errorf("%s: template %q capabilities %+v", c.want, spec.ChatTemplate, spec.Capabilities)
		}
		if !c.tools && !strings.Contains(out, "cannot offer native tool calls") {
			t.Errorf("%s: no note that tools are off:\n%s", c.want, out)
		}
	}
}

func TestModelBlessGGUFTemplateFlag(t *testing.T) {
	// An override wins, and a disagreement with the header is said out loud.
	out, err := bless(t, ggufRepo(ggufHeader("llama", 4096, chatmlText)), "--chat-template", "llama3")
	if err != nil {
		t.Fatal(err)
	}
	if got := blessedSpec(t, out).ChatTemplate; got != "llama3" {
		t.Errorf("chatTemplate = %q", got)
	}
	if !strings.Contains(out, "looks like chatml; using llama3") {
		t.Errorf("the disagreement was not reported:\n%s", out)
	}

	// With no usable template in the file a name must be given, never guessed.
	for name, header := range map[string][]byte{
		"none":    ggufHeader("llama", 4096, ""),
		"unknown": ggufHeader("llama", 4096, unknownFormat),
	} {
		if out, err := bless(t, ggufRepo(header)); err == nil || !strings.Contains(err.Error(), "--chat-template") {
			t.Errorf("%s: err = %v\n%s", name, err, out)
		}
		out, err := bless(t, ggufRepo(header), "--chat-template", "gemma4")
		if err != nil || blessedSpec(t, out).ChatTemplate != "gemma4" {
			t.Errorf("%s with flag: err = %v", name, err)
		}
	}

	if _, err := bless(t, ggufRepo(ggufHeader("llama", 4096, chatmlText)), "--chat-template", "../../etc/passwd"); err == nil ||
		!strings.Contains(err.Error(), "not a trusted template") {
		t.Errorf("an unknown template name must be refused, got %v", err)
	}
}

func TestModelBlessGGUFLeavesOutAuxiliaryAndSplitFiles(t *testing.T) {
	sha := strings.Repeat("cd", 32)
	repo := ggufRepo(ggufHeader("gptoss", 4096, ossTemplate),
		blessLFS("mmproj-F16.gguf", 900_000_000, sha),
		blessLFS("mtp-Q4_K_M.gguf", 300_000_000, sha),
		blessLFS("eagle3-draft.gguf", 300_000_000, sha),
		blessLFS("model-Q8_0-00001-of-00002.gguf", 5_000_000_000, sha),
		blessLFS("model-Q8_0-00002-of-00002.gguf", 5_000_000_000, sha),
		blessLFS("BF16/model-BF16.gguf", 40_000_000_000, sha),
	)
	out, err := bless(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	spec := blessedSpec(t, out)
	if len(spec.Quants) != 1 || spec.Quants[0].Ref != "model-MXFP4.gguf" {
		t.Fatalf("quants = %+v, want only the standalone model", spec.Quants)
	}
	for _, name := range []string{"mmproj-F16.gguf", "mtp-Q4_K_M.gguf", "eagle3-draft.gguf", "model-Q8_0-00001-of-00002.gguf", "BF16/model-BF16.gguf"} {
		if !strings.Contains(out, "skipping "+name) {
			t.Errorf("no note that %s was left out:\n%s", name, out)
		}
	}
}

func TestModelBlessGGUFQuantSelection(t *testing.T) {
	sha := strings.Repeat("ee", 32)
	repo := ggufRepo(ggufHeader("llama", 4096, chatmlText),
		blessLFS("model-Q4_K_M.gguf", 4_000_000_000, sha),
		blessLFS("model-Q8_0.gguf", 8_000_000_000, sha),
	)
	// The first quant by name is the one whose header is read.
	repo.files["model-Q4_K_M.gguf"] = repo.files["model-MXFP4.gguf"]

	out, err := bless(t, repo)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(blessedSpec(t, out).Quants); n != 3 {
		t.Errorf("quants = %d, want every model file", n)
	}
	out, err = bless(t, repo, "--quant", "q4_k_m")
	if err != nil {
		t.Fatal(err)
	}
	if q := blessedSpec(t, out).Quants; len(q) != 1 || q[0].Name != "Q4_K_M" {
		t.Errorf("quants = %+v", q)
	}
	if _, err := bless(t, repo, "--quant", "Q2_K"); err == nil || !strings.Contains(err.Error(), "available: ") {
		t.Errorf("an unknown quant must list the available ones, got %v", err)
	}
}

func TestModelBlessGGUFRefusals(t *testing.T) {
	good := ggufHeader("gptoss", 4096, ossTemplate)

	noLicense := ggufRepo(good)
	noLicense.info = map[string]any{"id": "acme/tiny-20b-GGUF"}
	if _, err := bless(t, noLicense); err == nil || !strings.Contains(err.Error(), "no license") {
		t.Errorf("no license: %v", err)
	}
	out, err := bless(t, noLicense, "--license", "mit")
	if err != nil || blessedSpec(t, out).License != "mit" {
		t.Errorf("license override: %v", err)
	}

	missing := ggufRepo(good)
	delete(missing.files, "model-MXFP4.gguf")
	if _, err := bless(t, missing); err == nil || !strings.Contains(err.Error(), "read the GGUF header") {
		t.Errorf("unreadable header: %v", err)
	}

	notGGUF := ggufRepo([]byte("this is not a gguf file at all"))
	if _, err := bless(t, notGGUF); err == nil || !strings.Contains(err.Error(), "not a GGUF file") {
		t.Errorf("wrong magic: %v", err)
	}

	onlySplit := blessRepo{
		info: map[string]any{"cardData": map[string]any{"license": "mit"}},
		tree: []map[string]any{blessLFS("m-00001-of-00002.gguf", 10, strings.Repeat("11", 32))},
	}
	if _, err := bless(t, onlySplit); err == nil || !strings.Contains(err.Error(), "no standalone GGUF") {
		t.Errorf("only split files: %v", err)
	}

	noContext := ggufRepo(ggufHeader("gptoss", 0, ossTemplate))
	out, err = bless(t, noContext)
	if err != nil || blessedSpec(t, out).ContextTokens != 0 || !strings.Contains(out, "does not declare a context length") {
		t.Errorf("missing context: err = %v\n%s", err, out)
	}
}

func TestSelectGGUFModels(t *testing.T) {
	sha := "aa"
	files := []huggingface.File{
		{Path: "b-Q8_0.gguf", SHA256: sha},
		{Path: "a-Q4_K_M.gguf", SHA256: sha},
		{Path: "a-imatrix.gguf", SHA256: sha},
		{Path: "mmproj-BF16.gguf", SHA256: sha},
		{Path: "x/y.gguf", SHA256: sha},
		{Path: "no-digest.gguf"},
		{Path: "README.md"},
	}
	models, skipped := selectGGUFModels(files)
	if len(models) != 2 || models[0].Path != "a-Q4_K_M.gguf" || models[1].Path != "b-Q8_0.gguf" {
		t.Errorf("models = %+v", models)
	}
	if len(skipped) != 4 {
		t.Errorf("skipped = %+v", skipped)
	}
}

func TestGGUFQuantName(t *testing.T) {
	for file, want := range map[string]string{
		"gpt-oss-20b-MXFP4.gguf":               "MXFP4",
		"gemma-4-12b-it-qat-q4_0.gguf":         "Q4_0",
		"Qwen3.5-9B-Q4_K_M.gguf":               "Q4_K_M",
		"Qwen3.5-9B-UD-Q4_K_XL.gguf":           "Q4_K_XL",
		"model-IQ3_XS.gguf":                    "IQ3_XS",
		"model-BF16.gguf":                      "BF16",
		"model.F16.gguf":                       "F16",
		"plain.gguf":                           "plain",
		"gpt-oss-20b-mxfp4.gguf":               "MXFP4",
		"Meta-Llama-3.1-8B-Instruct-Q8_0.gguf": "Q8_0",
	} {
		if got := ggufQuantName(file); got != want {
			t.Errorf("ggufQuantName(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestGGUFModelName(t *testing.T) {
	for in, want := range map[string]string{"x-GGUF": "x", "x-gguf": "x", "x": "x", "-gguf": "-gguf", "GGUF": "GGUF"} {
		if got := ggufModelName(in); got != want {
			t.Errorf("ggufModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadGGUFHeaderRefusesPlainHTTP(t *testing.T) {
	if _, err := readGGUFHeader(context.Background(), "http://example.com/m.gguf"); err == nil {
		t.Fatal("a non-https URL must be refused")
	}
}

// TestModelBlessSafetensorsCountsTheWeightsOnce is the regression for a model published
// with its weights twice (the root shards and an original/ copy) and a stray extra shard
// the weight index does not name. Only the files the index names are carried, so the size
// matches what a runtime loads.
func TestModelBlessSafetensorsCountsTheWeightsOnce(t *testing.T) {
	index, _ := json.Marshal(map[string]any{"weight_map": map[string]string{
		"a": "model-00001-of-00002.safetensors",
		"b": "model-00001-of-00002.safetensors",
		"c": "model-00002-of-00002.safetensors",
	}})
	config := []byte(`{"max_position_embeddings":131072}`)
	hub, dl := newBlessHub(t, blessRepo{
		info: map[string]any{"id": "acme/tiny-20b", "cardData": map[string]any{"license": "apache-2.0"}},
		tree: []map[string]any{
			blessLFS("model-00000-of-00002.safetensors", 4_000_000_000, strings.Repeat("00", 32)),
			blessLFS("model-00001-of-00002.safetensors", 4_800_000_000, strings.Repeat("01", 32)),
			blessLFS("model-00002-of-00002.safetensors", 4_100_000_000, strings.Repeat("02", 32)),
			blessLFS("original/model.safetensors", 13_000_000_000, strings.Repeat("03", 32)),
			blessSmall("original/config.json", 10),
			blessSmall(weightIndexName, int64(len(index))),
			blessSmall("config.json", int64(len(config))),
		},
		files: map[string][]byte{weightIndexName: index, "config.json": config},
	})
	var out bytes.Buffer
	if err := modelBless(context.Background(), hub, dl, []string{"acme/tiny-20b"}, &out); err != nil {
		t.Fatalf("modelBless: %v\n%s", err, out.String())
	}
	q := blessedSpec(t, out.String()).Quants[0]
	if want := int64(4_800_000_000 + 4_100_000_000 + len(index) + len(config)); q.SizeBytes != want {
		t.Errorf("size = %d, want %d", q.SizeBytes, want)
	}
	for _, f := range q.Files {
		if strings.Contains(f.Name, "/") || f.Name == "model-00000-of-00002.safetensors" {
			t.Errorf("%s is not part of the model and must not be carried", f.Name)
		}
	}
	if len(q.Files) != 4 {
		t.Errorf("files = %+v", q.Files)
	}
}

func TestModelBlessSafetensorsRefusesAnInconsistentIndex(t *testing.T) {
	for name, index := range map[string]string{
		"not json":      `{`,
		"empty":         `{"weight_map":{}}`,
		"missing shard": `{"weight_map":{"a":"model-00009-of-00009.safetensors"}}`,
	} {
		hub, dl := newBlessHub(t, blessRepo{
			info: map[string]any{"cardData": map[string]any{"license": "mit"}},
			tree: []map[string]any{
				blessLFS("model-00001-of-00001.safetensors", 10, strings.Repeat("01", 32)),
				blessSmall(weightIndexName, int64(len(index))),
			},
			files: map[string][]byte{weightIndexName: []byte(index)},
		})
		var out bytes.Buffer
		if err := modelBless(context.Background(), hub, dl, []string{"acme/tiny"}, &out); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

func chatTemplateConfig(tmpl string) []byte {
	b, _ := json.Marshal(map[string]string{"chat_template": tmpl})
	return b
}

func TestModelBlessSafetensorsDetectsTheTemplate(t *testing.T) {
	cases := map[string]struct {
		files     map[string][]byte
		want      string
		reasoning bool
		note      bool
	}{
		"template file": {
			files:     map[string][]byte{chatTemplateFile: []byte(ossTemplate)},
			want:      "gpt-oss",
			reasoning: true,
		},
		"tokenizer string": {
			files: map[string][]byte{"tokenizer_config.json": chatTemplateConfig(chatmlText)},
			want:  "chatml",
		},
		"tokenizer list": {
			files: map[string][]byte{"tokenizer_config.json": []byte(`{"chat_template":[{"name":"default","template":"<|start_header_id|>"},{"name":"tool_use","template":"x <think>"}]}`)},
			want:  "llama3", reasoning: true,
		},
		"nothing declared": {
			files: map[string][]byte{"tokenizer_config.json": []byte(`{"model_max_length":1}`)},
			want:  "chatml", note: true,
		},
		"garbage config": {
			files: map[string][]byte{"tokenizer_config.json": []byte(`{`)},
			want:  "chatml", note: true,
		},
		"template of the wrong type": {
			files: map[string][]byte{"tokenizer_config.json": []byte(`{"chat_template":7}`)},
			want:  "chatml", note: true,
		},
	}
	for name, c := range cases {
		tree := []map[string]any{blessLFS("model.safetensors", 10, strings.Repeat("01", 32))}
		for f, b := range c.files {
			tree = append(tree, blessSmall(f, int64(len(b))))
		}
		hub, dl := newBlessHub(t, blessRepo{
			info:  map[string]any{"cardData": map[string]any{"license": "mit"}},
			tree:  tree,
			files: c.files,
		})
		var out bytes.Buffer
		if err := modelBless(context.Background(), hub, dl, []string{"acme/tiny"}, &out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		spec := blessedSpec(t, out.String())
		if spec.ChatTemplate != c.want || spec.Capabilities.Reasoning != c.reasoning {
			t.Errorf("%s: template %q reasoning %v", name, spec.ChatTemplate, spec.Capabilities.Reasoning)
		}
		if got := strings.Contains(out.String(), "defaulting to chatml"); got != c.note {
			t.Errorf("%s: default note = %v", name, got)
		}
	}
}

func TestModelBlessRefusesUnknownTemplateFlag(t *testing.T) {
	hub, dl := newBlessHub(t, blessRepo{info: map[string]any{}, tree: nil})
	var out bytes.Buffer
	err := modelBless(context.Background(), hub, dl, []string{"--chat-template", "evil", "acme/tiny"}, &out)
	if err == nil || !strings.Contains(err.Error(), "not a trusted template") {
		t.Fatalf("err = %v", err)
	}
}

func TestPruneToWeightIndexKeepsNonWeights(t *testing.T) {
	files := []huggingface.File{
		{Path: "a.safetensors"}, {Path: "b.safetensors"}, {Path: "config.json"},
	}
	kept, err := pruneToWeightIndex(files, []byte(`{"weight_map":{"x":"a.safetensors"}}`))
	if err != nil || len(kept) != 2 || kept[0].Path != "a.safetensors" || kept[1].Path != "config.json" {
		t.Fatalf("kept = %+v err = %v", kept, err)
	}
}
