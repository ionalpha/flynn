package modelsource

import (
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

func catalogIs(ids ...string) func(string) bool {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return func(s string) bool { return set[s] }
}

func TestParseClassifiesReferences(t *testing.T) {
	isCat := catalogIs("qwen2.5:0.5b-instruct")
	cases := []struct {
		ref      string
		wantKind Kind
		check    func(Source) bool
	}{
		{"qwen2.5:0.5b-instruct", KindCatalog, func(s Source) bool { return s.CatalogID == "qwen2.5:0.5b-instruct" }},
		{"hf:Qwen/Qwen2.5-0.5B-Instruct-GGUF", KindHuggingFace, func(s Source) bool {
			return s.Owner == "Qwen" && s.Repo == "Qwen2.5-0.5B-Instruct-GGUF" && s.File == ""
		}},
		{"hf:Qwen/Qwen2.5-0.5B-Instruct-GGUF/model.gguf", KindHuggingFace, func(s Source) bool { return s.File == "model.gguf" }},
		{"https://huggingface.co/Qwen/Q/resolve/main/model.gguf", KindHuggingFace, func(s Source) bool { return s.Owner == "Qwen" && s.Repo == "Q" && s.File == "model.gguf" }},
		{"https://example.test/model.gguf", KindURL, func(s Source) bool { return s.URL == "https://example.test/model.gguf" }},
		{"/home/user/model.gguf", KindFile, func(s Source) bool { return s.Path == "/home/user/model.gguf" }},
		{`C:\models\model.gguf`, KindFile, func(s Source) bool { return s.Path == `C:\models\model.gguf` }},
	}
	for _, c := range cases {
		s, err := Parse(c.ref, isCat)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.ref, err)
		}
		if s.Kind != c.wantKind || !c.check(s) {
			t.Fatalf("Parse(%q) = %+v, kind/fields mismatch (want kind %d)", c.ref, s, c.wantKind)
		}
	}
}

func TestParseRejectsBad(t *testing.T) {
	for _, ref := range []string{"", "   ", "hf:onlyowner", "hf:/repo", "hf:owner/"} {
		if _, err := Parse(ref, nil); err == nil {
			t.Fatalf("Parse(%q) should have failed", ref)
		}
	}
}

func TestClassifyTrust(t *testing.T) {
	known := func(owner string) bool { return owner == "Qwen" }
	cases := []struct {
		src  Source
		want sandbox.Trust
	}{
		{Source{Kind: KindCatalog, CatalogID: "x"}, sandbox.TrustTrusted},
		{Source{Kind: KindHuggingFace, Owner: "Qwen", Repo: "r"}, sandbox.TrustSemi},
		{Source{Kind: KindHuggingFace, Owner: "rando", Repo: "r"}, sandbox.TrustUntrusted},
		{Source{Kind: KindURL, URL: "https://x/y.gguf"}, sandbox.TrustUntrusted},
		{Source{Kind: KindFile, Path: "/x.gguf"}, sandbox.TrustUntrusted},
	}
	for _, c := range cases {
		got := Classify(c.src, known)
		if got.Trust != c.want {
			t.Fatalf("Classify(%+v).Trust = %v, want %v", c.src, got.Trust, c.want)
		}
		if got.Reason == "" {
			t.Fatalf("Classify(%+v) must carry a plain-language reason", c.src)
		}
	}
}

func TestFormatGuard(t *testing.T) {
	allowed := []string{"model.gguf", "weights.safetensors", "Model.GGUF"}
	for _, n := range allowed {
		if err := CheckRunnableFormat(n); err != nil {
			t.Fatalf("CheckRunnableFormat(%q) should allow, got %v", n, err)
		}
	}
	refused := []string{"model.bin", "weights.pt", "x.pth", "x.ckpt", "x.pkl", "arch.zip", "arch.tar", "blob.gz", "noext", "model.onnx"}
	for _, n := range refused {
		if err := CheckRunnableFormat(n); err == nil {
			t.Fatalf("CheckRunnableFormat(%q) should refuse", n)
		}
	}
}

func TestKnownPublisherCaseInsensitiveAndExtra(t *testing.T) {
	if !KnownPublisher("Qwen") || !KnownPublisher("qwen") || !KnownPublisher("MISTRALAI") {
		t.Fatal("recognized publishers must match case-insensitively")
	}
	if KnownPublisher("some-random-user") {
		t.Fatal("an unlisted owner must not be recognized")
	}
	if !KnownPublisher("MyOrg", "myorg") {
		t.Fatal("an extra publisher must be recognized")
	}
	if KnownPublisher("") {
		t.Fatal("empty owner is never a publisher")
	}
}
