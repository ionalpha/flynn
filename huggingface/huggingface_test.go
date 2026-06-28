package huggingface

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))
}

func TestTreeReturnsFilesWithLFSDigests(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/Qwen/Qwen2.5-7B-Instruct-AWQ/tree/main" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("recursive") != "true" {
			t.Errorf("expected recursive=true, got %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
			{"type":"file","path":"config.json","size":841,"oid":"abc"},
			{"type":"file","path":"model.safetensors","size":100,"oid":"x","lfs":{"oid":"deadbeef","size":3996422976}},
			{"type":"directory","path":"subdir"}
		]`))
	})

	files, err := c.Tree(context.Background(), "Qwen/Qwen2.5-7B-Instruct-AWQ")
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (directory dropped), got %d", len(files))
	}
	var weights File
	for _, f := range files {
		if f.Path == "model.safetensors" {
			weights = f
		}
	}
	if !weights.LFS || weights.SHA256 != "deadbeef" {
		t.Errorf("LFS digest not taken from lfs.oid: %+v", weights)
	}
	if weights.Size != 3996422976 {
		t.Errorf("LFS size should override the tree size, got %d", weights.Size)
	}
	// A small non-LFS file carries no usable content digest.
	for _, f := range files {
		if f.Path == "config.json" && (f.SHA256 != "" || f.LFS) {
			t.Errorf("non-LFS file must not claim a content digest: %+v", f)
		}
	}
}

func TestTreeNotFoundIsTerminal(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := c.Tree(context.Background(), "nobody/nope"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestTreeEmptyIsError(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"type":"directory","path":"d"}]`))
	})
	if _, err := c.Tree(context.Background(), "owner/empty"); err == nil {
		t.Fatal("expected empty-tree error")
	}
}

func TestInfoReadsLicenseAndGated(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"Qwen/X","author":"Qwen","tags":["text-generation"],"gated":"manual","cardData":{"license":"apache-2.0"}}`))
	})
	info, err := c.Info(context.Background(), "Qwen/X")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.License != "apache-2.0" {
		t.Errorf("license = %q", info.License)
	}
	if !info.Gated {
		t.Errorf("gated should be true for %q", "manual")
	}
}

func TestInfoGatedFalse(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"a/b","gated":false,"cardData":{"license":"mit"}}`))
	})
	info, err := c.Info(context.Background(), "a/b")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Gated {
		t.Error("gated should be false")
	}
}

func TestFileURLResolvesMain(t *testing.T) {
	c := New(WithBaseURL("https://example.test"))
	got := c.FileURL("Qwen/X", "model.safetensors")
	want := "https://example.test/Qwen/X/resolve/main/model.safetensors"
	if got != want {
		t.Errorf("FileURL = %q, want %q", got, want)
	}
}

func TestCheckRepoRejectsMalformed(t *testing.T) {
	c := New()
	for _, bad := range []string{"", "noslash", "a/b/c", "../etc", "a/b?x=1"} {
		if _, err := c.Tree(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "huggingface") {
			t.Errorf("expected rejection for %q, got %v", bad, err)
		}
	}
}
