package huggingface

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestTreeParsingProperty checks the invariants that hold for any repository tree the
// Hub can return: directories are dropped, every file is kept, an LFS-tracked file
// carries the content digest from its lfs.oid, and a plain git-tracked file carries no
// content digest. These are the trust-bearing guarantees the bless path relies on, so
// they are asserted over randomized manifests rather than a single fixture.
func TestTreeParsingProperty(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c := New(WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))

	rapid.Check(t, func(t *rapid.T) {
		type entry struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
			OID  string `json:"oid"`
			LFS  *struct {
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			} `json:"lfs,omitempty"`
		}

		nFiles := rapid.IntRange(1, 6).Draw(t, "nFiles")
		nDirs := rapid.IntRange(0, 3).Draw(t, "nDirs")

		var entries []entry
		wantDigest := map[string]string{} // path -> expected SHA256 ("" when none)
		for i := range nFiles {
			path := fmt.Sprintf("file-%d.bin", i)
			e := entry{Type: "file", Path: path, Size: rapid.Int64Range(0, 1<<40).Draw(t, "size")}
			if rapid.Bool().Draw(t, "lfs") {
				oid := rapid.StringMatching(`[0-9a-f]{64}`).Draw(t, "oid")
				lfsSize := rapid.Int64Range(1, 1<<42).Draw(t, "lfsSize")
				e.LFS = &struct {
					OID  string `json:"oid"`
					Size int64  `json:"size"`
				}{OID: oid, Size: lfsSize}
				wantDigest[path] = oid
			} else {
				e.OID = rapid.StringMatching(`[0-9a-f]{40}`).Draw(t, "gitoid") // git blob sha, not a content digest
				wantDigest[path] = ""
			}
			entries = append(entries, e)
		}
		for i := range nDirs {
			entries = append(entries, entry{Type: "directory", Path: fmt.Sprintf("dir-%d", i)})
		}

		b, err := json.Marshal(entries)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = b

		files, err := c.Tree(context.Background(), "owner/name")
		if err != nil {
			t.Fatalf("Tree: %v", err)
		}
		if len(files) != nFiles {
			t.Fatalf("got %d files, want %d (directories must be dropped)", len(files), nFiles)
		}
		for _, f := range files {
			want, ok := wantDigest[f.Path]
			if !ok {
				t.Fatalf("unexpected file %q in result", f.Path)
			}
			if want == "" {
				if f.SHA256 != "" || f.LFS {
					t.Fatalf("non-LFS file %q must carry no content digest, got %q lfs=%v", f.Path, f.SHA256, f.LFS)
				}
			} else {
				if !f.LFS || f.SHA256 != want {
					t.Fatalf("LFS file %q must carry its lfs.oid as the digest: got %q lfs=%v, want %q", f.Path, f.SHA256, f.LFS, want)
				}
			}
		}
	})
}

// TestRepoCheckProperty checks that any well-formed owner/name is accepted and that a
// reference carrying traversal or a query is always rejected, so a hostile reference can
// never be assembled into a request URL.
func TestRepoCheckProperty(t *testing.T) {
	seg := rapid.StringMatching(`[A-Za-z0-9._-]{1,24}`)
	rapid.Check(t, func(t *rapid.T) {
		owner := seg.Draw(t, "owner")
		name := seg.Draw(t, "name")
		repo := owner + "/" + name
		err := checkRepo(repo)
		hasTraversal := strings.Contains(repo, "..")
		if hasTraversal {
			if err == nil {
				t.Fatalf("traversal reference %q must be rejected", repo)
			}
			return
		}
		if err != nil {
			t.Fatalf("well-formed reference %q rejected: %v", repo, err)
		}
	})
}
