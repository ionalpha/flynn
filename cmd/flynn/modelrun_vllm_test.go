package main

import (
	"testing"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/inference/launch"
)

func st(format catalog.Format, name string, size int64) catalog.Quant {
	return catalog.Quant{Name: name, Format: format, SizeBytes: size}
}

func TestSelectServeQuant(t *testing.T) {
	gguf := st(catalog.FormatGGUF, "Q4_K_M", 500)
	safet := st(catalog.FormatSafetensors, "fp16", 1000)

	t.Run("safetensors on a gpu host picks vLLM", func(t *testing.T) {
		m := catalog.ModelSpec{ID: "m", Quants: []catalog.Quant{safet}}
		q, eng, err := selectServeQuant(m, true)
		if err != nil || eng != launch.EngineVLLM || q.Format != catalog.FormatSafetensors {
			t.Fatalf("got q=%v eng=%v err=%v", q.Name, eng, err)
		}
	})

	t.Run("safetensors-only with no gpu is refused", func(t *testing.T) {
		m := catalog.ModelSpec{ID: "m", Quants: []catalog.Quant{safet}}
		if _, _, err := selectServeQuant(m, false); err == nil {
			t.Fatal("a vLLM-only model must be refused without a GPU container path")
		}
	})

	t.Run("falls back to gguf when no gpu", func(t *testing.T) {
		m := catalog.ModelSpec{ID: "m", Quants: []catalog.Quant{safet, gguf}}
		q, eng, err := selectServeQuant(m, false)
		if err != nil || eng != launch.EngineLlamaCpp || q.Format != catalog.FormatGGUF {
			t.Fatalf("expected the GGUF fallback, got q=%v eng=%v err=%v", q.Name, eng, err)
		}
	})

	t.Run("unservable format is skipped", func(t *testing.T) {
		m := catalog.ModelSpec{ID: "m", Quants: []catalog.Quant{st(catalog.FormatPickle, "bad", 10)}}
		if _, _, err := selectServeQuant(m, true); err == nil {
			t.Fatal("a code-executing format must never be selected")
		}
	})
}

func TestVLLMQuantScheme(t *testing.T) {
	cases := map[string]string{
		"fp16": "", "Q4-AWQ": "awq", "gptq-int4": "gptq",
		"NVFP4": "modelopt_fp4", "fp8-dynamic": "fp8",
	}
	for name, want := range cases {
		if got := vllmQuantScheme(catalog.Quant{Name: name}); got != want {
			t.Fatalf("vllmQuantScheme(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestModelDirNameIsFilesystemSafe(t *testing.T) {
	got := modelDirName("vllm:qwen2.5-0.5b/instruct", catalog.Quant{Name: "fp16"})
	for _, r := range got {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_'
		if !ok {
			t.Fatalf("model dir name has an unsafe character %q: %s", r, got)
		}
	}
}
