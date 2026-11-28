package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ionalpha/flynn/sandbox"
)

var errBroken = errors.New("broken")

// faulty is a local sandbox that fails on the paths a case names.
type faulty struct {
	*sandbox.Local
	statErr, readErr, listErr map[string]error
	pipe                      string
	afterRead                 func()
}

func (f faulty) Stat(ctx context.Context, p string) (fs.FileInfo, error) {
	if err := f.statErr[p]; err != nil {
		return nil, err
	}
	info, err := f.Local.Stat(ctx, p)
	if err == nil && p == f.pipe {
		info = pipeInfo{info}
	}
	return info, err
}

func (f faulty) ReadDir(ctx context.Context, p string) ([]fs.DirEntry, error) {
	if err := f.listErr[p]; err != nil {
		return nil, err
	}
	return f.Local.ReadDir(ctx, p)
}

func (f faulty) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if err := f.readErr[p]; err != nil {
		return nil, err
	}
	if f.afterRead != nil {
		defer f.afterRead()
	}
	return f.Local.ReadFile(ctx, p)
}

type pipeInfo struct{ fs.FileInfo }

func (pipeInfo) Mode() fs.FileMode { return fs.ModeNamedPipe }

type brokenWalk struct{ sandbox.Sandbox }

func (brokenWalk) Walk(context.Context, string) ([]string, error) { return nil, errBroken }

func faultTree(t *testing.T) *sandbox.Local {
	t.Helper()
	_, sb := newSet(t)
	seed(t, sb, "a.txt", "needle")
	seed(t, sb, "sub/b.txt", "needle")
	local, ok := sb.(*sandbox.Local)
	if !ok {
		t.Fatalf("sandbox is %T", sb)
	}
	return local
}

func call(ctx context.Context, s *Set, name, args string) (string, error) {
	if name == "grep" {
		return grepTool{s}.Invoke(ctx, json.RawMessage(args))
	}
	return globTool{s}.Invoke(ctx, json.RawMessage(args))
}

func TestSearchBackendFailures(t *testing.T) {
	sb := faultTree(t)
	broken := map[string]error{".": errBroken}
	denied := map[string]error{"sub": sandbox.ErrDenied, "a.txt": sandbox.ErrDenied}
	for _, tc := range []struct {
		name, tool, args string
		sb               sandbox.Sandbox
		want             error  // nil means the call succeeds
		out              string // expected result when it succeeds
	}{
		{"root listing fails", "grep", `{"pattern":"needle"}`, faulty{Local: sb, listErr: broken}, errBroken, ""},
		{"glob listing fails", "glob", `{"pattern":"**"}`, faulty{Local: sb, listErr: broken}, errBroken, ""},
		{"subdirectory skipped", "grep", `{"pattern":"needle"}`, faulty{Local: sb, listErr: map[string]error{"sub": errBroken}}, nil, "a.txt:1:needle"},
		{"unreadable file skipped", "grep", `{"pattern":"needle"}`, faulty{Local: sb, readErr: map[string]error{"a.txt": errBroken}}, nil, "sub/b.txt:1:needle"},
		{"named file unreadable", "grep", `{"pattern":"needle","path":"a.txt"}`, faulty{Local: sb, readErr: map[string]error{"a.txt": errBroken}}, errBroken, ""},
		{"named pipe", "grep", `{"pattern":"needle","path":"a.txt"}`, faulty{Local: sb, pipe: "a.txt"}, nil, "not a regular file"},
		{"walk fails", "grep", `{"pattern":"needle"}`, brokenWalk{sb}, errBroken, ""},
		{"named path denied", "glob", `{"pattern":"a.txt"}`, faulty{Local: sb, statErr: denied}, sandbox.ErrDenied, ""},
		{"base denied", "glob", `{"pattern":"sub/*"}`, faulty{Local: sb, statErr: denied}, sandbox.ErrDenied, ""},
		{"missing in listing", "grep", `{"pattern":"needle","path":"nope"}`, listOnly{sb}, fs.ErrNotExist, ""},
		{"empty glob", "glob", `{"pattern":""}`, sb, nil, "no matches"},
		{"slash is the root", "grep", `{"pattern":"needle","path":"/","glob":"a.txt"}`, sb, nil, "a.txt:1:needle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := call(context.Background(), New(tc.sb), tc.tool, tc.args)
			switch {
			case tc.name == "named pipe":
				if err == nil || !strings.Contains(err.Error(), tc.out) {
					t.Errorf("err = %v, want %q", err, tc.out)
				}
			case tc.want != nil:
				if !errors.Is(err, tc.want) {
					t.Errorf("err = %v, want %v", err, tc.want)
				}
			case err != nil || out != tc.out:
				t.Errorf("got %q, %v; want %q", out, err, tc.out)
			}
		})
	}
}

func TestSearchRejectsMalformedArguments(t *testing.T) {
	s := New(faultTree(t))
	for _, tool := range []string{"grep", "glob"} {
		if _, err := call(context.Background(), s, tool, `{`); err == nil {
			t.Errorf("%s accepted malformed JSON", tool)
		}
	}
	if _, err := call(context.Background(), s, "grep", `{"pattern":"("}`); err == nil {
		t.Error("grep accepted an invalid expression")
	}
}

// An empty file gives the matcher nothing to check the context in, so the walk
// itself has to notice the cancel before the next entry.
func TestSearchCancelBetweenEntries(t *testing.T) {
	sb := faultTree(t)
	seed(t, sb, "a.txt", "")
	seed(t, sb, "b.txt", "needle")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := call(ctx, New(faulty{Local: sb, afterRead: cancel}), "grep", `{"pattern":"needle"}`)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A listing backend cannot report a size, so the limit applies after the read.
func TestGrepSkipsLargeFileFromListing(t *testing.T) {
	_, sb := newSet(t)
	seed(t, sb, "big.txt", strings.Repeat("needle\n", maxSearchedFile/7+1))
	out, err := call(context.Background(), New(listOnly{sb}), "grep", `{"pattern":"needle"}`)
	if err != nil || !strings.Contains(out, "skipped 1 files") || !strings.Contains(out, "big.txt") {
		t.Errorf("got %.200q, %v", out, err)
	}
}

func TestListTreeListsOnce(t *testing.T) {
	tr := &listTree{sb: faultTree(t)}
	for _, p := range []string{".", "sub"} {
		if n, err := tr.stat(context.Background(), p); err != nil || !n.dir {
			t.Errorf("stat(%q) = %+v, %v", p, n, err)
		}
	}
}

func TestClipLineCutsAtRuneBoundary(t *testing.T) {
	got := clipLine([]byte("x" + strings.Repeat("é", maxLineBytes)))
	if !utf8.ValidString(got) || !strings.Contains(got, "more bytes]") {
		t.Errorf("clipLine = %q", got)
	}
}

func TestHitCountLeavesOutNotes(t *testing.T) {
	if n := hitCount("a:1:x\n--\nb:2:y\n" + notePrefix + "3 more]"); n != 2 {
		t.Errorf("hitCount = %d, want 2", n)
	}
}
