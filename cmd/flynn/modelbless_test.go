package main

import (
	"testing"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/huggingface"
)

func TestSelectServeFilesKeepsWeightsDropsDocs(t *testing.T) {
	files := []huggingface.File{
		{Path: "model-00001-of-00002.safetensors", Size: 4_000_000_000, SHA256: "a", LFS: true},
		{Path: "model-00002-of-00002.safetensors", Size: 1_500_000_000, SHA256: "b", LFS: true},
		{Path: "config.json", Size: 800},
		{Path: "tokenizer.json", Size: 7_000_000},
		{Path: "merges.txt", Size: 1_600_000},
		{Path: "tokenizer.model", Size: 500_000},
		{Path: "README.md", Size: 6000},
		{Path: "LICENSE", Size: 11000},
		{Path: ".gitattributes", Size: 1500},
		{Path: "figure.png", Size: 200000},
	}
	kept, format, err := selectServeFiles(files)
	if err != nil {
		t.Fatalf("selectServeFiles: %v", err)
	}
	if format != catalog.FormatSafetensors {
		t.Errorf("format = %q", format)
	}
	want := map[string]bool{
		"model-00001-of-00002.safetensors": true,
		"model-00002-of-00002.safetensors": true,
		"config.json":                      true,
		"tokenizer.json":                   true,
		"merges.txt":                       true,
		"tokenizer.model":                  true,
	}
	if len(kept) != len(want) {
		t.Fatalf("kept %d files, want %d: %+v", len(kept), len(want), kept)
	}
	for _, f := range kept {
		if !want[f.Path] {
			t.Errorf("unexpected kept file %q", f.Path)
		}
	}
}

func TestSelectServeFilesRefusesPickleOnly(t *testing.T) {
	files := []huggingface.File{
		{Path: "pytorch_model.bin", Size: 5_000_000_000, SHA256: "a", LFS: true},
		{Path: "config.json", Size: 800},
	}
	if _, _, err := selectServeFiles(files); err == nil {
		t.Fatal("expected refusal to bless a pickle-only model")
	}
}

func TestSelectServeFilesDropsStrayPickleWhenSafetensorsPresent(t *testing.T) {
	files := []huggingface.File{
		{Path: "model.safetensors", Size: 5_000_000_000, SHA256: "a", LFS: true},
		{Path: "pytorch_model.bin", Size: 5_000_000_000, SHA256: "b", LFS: true},
		{Path: "config.json", Size: 800},
	}
	kept, _, err := selectServeFiles(files)
	if err != nil {
		t.Fatalf("selectServeFiles: %v", err)
	}
	for _, f := range kept {
		if f.Path == "pytorch_model.bin" {
			t.Error("a pickle weight must never be carried into the manifest")
		}
	}
}

func TestParseModelConfigReadsContextAndQuant(t *testing.T) {
	cfg := parseModelConfig([]byte(`{"max_position_embeddings":32768,"quantization_config":{"quant_method":"AWQ"}}`))
	if cfg.ContextTokens != 32768 {
		t.Errorf("context = %d", cfg.ContextTokens)
	}
	if cfg.QuantMethod != "awq" {
		t.Errorf("quant = %q (should be lowercased)", cfg.QuantMethod)
	}
	if got := cfg.quantName(); got != "awq" {
		t.Errorf("quantName = %q", got)
	}
}

func TestParseModelConfigDefaultsToFP16WhenNoScheme(t *testing.T) {
	cfg := parseModelConfig([]byte(`{"max_position_embeddings":4096}`))
	if got := cfg.quantName(); got != "fp16" {
		t.Errorf("quantName = %q, want fp16", got)
	}
}

func TestParseModelConfigToleratesGarbage(t *testing.T) {
	if cfg := parseModelConfig([]byte("not json")); cfg.ContextTokens != 0 || cfg.QuantMethod != "" {
		t.Errorf("garbage config should yield zero values, got %+v", cfg)
	}
	if cfg := parseModelConfig(nil); cfg.ContextTokens != 0 {
		t.Errorf("nil config should yield zero values, got %+v", cfg)
	}
}

func TestParamsFromName(t *testing.T) {
	cases := map[string]float64{
		"Qwen2.5-7B-Instruct-AWQ": 7,
		"Qwen2.5-0.5B-Instruct":   0.5,
		"Llama-3.1-70B":           70,
		"some-model":              0,
	}
	for name, want := range cases {
		if got := paramsFromName(name); got != want {
			t.Errorf("paramsFromName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseHubRef(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		wantErr     bool
	}{
		{"hf:Qwen/Qwen2.5-7B-Instruct-AWQ", "Qwen", "Qwen2.5-7B-Instruct-AWQ", false},
		{"Qwen/Qwen2.5-7B-Instruct-AWQ", "Qwen", "Qwen2.5-7B-Instruct-AWQ", false},
		{"https://huggingface.co/Qwen/Qwen2.5-7B-Instruct-AWQ", "Qwen", "Qwen2.5-7B-Instruct-AWQ", false},
		{"not a ref", "", "", true},
	}
	for _, c := range cases {
		owner, repo, err := parseHubRef(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseHubRef(%q): expected error", c.in)
			}
			continue
		}
		if err != nil || owner != c.owner || repo != c.repo {
			t.Errorf("parseHubRef(%q) = %q,%q,%v; want %q,%q,nil", c.in, owner, repo, err, c.owner, c.repo)
		}
	}
}

func TestTakeValue(t *testing.T) {
	rest, v := takeValue([]string{"a", "--id", "vllm:x", "b"}, "--id")
	if v != "vllm:x" {
		t.Errorf("value = %q", v)
	}
	if len(rest) != 2 || rest[0] != "a" || rest[1] != "b" {
		t.Errorf("rest = %v", rest)
	}
	if _, v := takeValue([]string{"a"}, "--id"); v != "" {
		t.Errorf("absent flag should yield empty, got %q", v)
	}
	_, v = takeValueOr([]string{"a"}, "--chat-template", "chatml")
	if v != "chatml" {
		t.Errorf("default not applied: %q", v)
	}
}
