package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/fault"
)

// fakeTransport is an in-memory Transport: the mock backend the Remote mapping is
// tested against, with no live cloud calls. Optional error fields inject backend
// failures to prove they surface unchanged.
type fakeTransport struct {
	mu       sync.Mutex
	files    map[string][]byte
	execFn   func(line string) (ExecResult, error)
	readErr  error
	listErr  error
	closed   bool
	lastExec string
}

func newFakeTransport() *fakeTransport { return &fakeTransport{files: map[string][]byte{}} }

func (f *fakeTransport) Exec(_ context.Context, line string) (ExecResult, error) {
	f.mu.Lock()
	f.lastExec = line
	f.mu.Unlock()
	if f.execFn != nil {
		return f.execFn(line)
	}
	return ExecResult{Output: "ran: " + line}, nil
}

func (f *fakeTransport) ReadFile(_ context.Context, p string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), b...), nil
}

func (f *fakeTransport) WriteFile(_ context.Context, p string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = append([]byte(nil), data...)
	return nil
}

func (f *fakeTransport) List(_ context.Context, dir string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for p := range f.files {
		if dir == "." || p == dir || strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeTransport) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func TestRemoteFileRoundTrip(t *testing.T) {
	tr := newFakeTransport()
	r := NewRemote(tr)
	if err := r.WriteFile(context.Background(), "dir/a.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadFile(context.Background(), "dir/a.txt")
	if err != nil || string(got) != "hi" {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestRemoteDeniesEscape(t *testing.T) {
	tr := newFakeTransport()
	r := NewRemote(tr)
	ctx := context.Background()
	for _, p := range []string{"../escape", "/etc/passwd", "a/../../b"} {
		if err := r.WriteFile(ctx, p, []byte("x")); !errors.Is(err, ErrDenied) {
			t.Fatalf("WriteFile(%q) err = %v, want ErrDenied", p, err)
		}
		if _, err := r.ReadFile(ctx, p); !errors.Is(err, ErrDenied) {
			t.Fatalf("ReadFile(%q) err = %v, want ErrDenied", p, err)
		}
		if _, err := r.Walk(ctx, p); !errors.Is(err, ErrDenied) {
			t.Fatalf("Walk(%q) err = %v, want ErrDenied", p, err)
		}
	}
	if len(tr.files) != 0 {
		t.Fatalf("a denied path still reached the backend: %v", tr.files)
	}
}

func TestRemoteExecDelegates(t *testing.T) {
	tr := newFakeTransport()
	tr.execFn = func(string) (ExecResult, error) { return ExecResult{Output: "out", ExitCode: 3}, nil }
	res, err := NewRemote(tr).Exec(context.Background(), Command{Line: "do thing"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "out" || res.ExitCode != 3 {
		t.Fatalf("exec result = %+v", res)
	}
	if tr.lastExec != "do thing" {
		t.Fatalf("transport saw %q", tr.lastExec)
	}
}

func TestRemoteGlobAndWalk(t *testing.T) {
	tr := newFakeTransport()
	r := NewRemote(tr)
	ctx := context.Background()
	for _, p := range []string{"a.txt", "b.go", "sub/c.go"} {
		if err := r.WriteFile(ctx, p, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	glob, err := r.Glob(ctx, "*.go")
	if err != nil || !reflect.DeepEqual(glob, []string{"b.go"}) {
		t.Fatalf("Glob(*.go) = %v, %v (want [b.go]; * does not cross /)", glob, err)
	}
	walk, err := r.Walk(ctx, ".")
	if err != nil || !reflect.DeepEqual(walk, []string{"a.txt", "b.go", "sub/c.go"}) {
		t.Fatalf("Walk(.) = %v, %v", walk, err)
	}
}

func TestRemoteCloseTearsDown(t *testing.T) {
	tr := newFakeTransport()
	if err := NewRemote(tr).Close(); err != nil {
		t.Fatal(err)
	}
	if !tr.closed {
		t.Fatal("Close did not tear down the transport")
	}
}

func TestRemotePropagatesBackendError(t *testing.T) {
	boom := fault.New(fault.Transient, "backend_down", "try later")
	tr := newFakeTransport()
	tr.readErr, tr.listErr = boom, boom
	r := NewRemote(tr)
	ctx := context.Background()
	if _, err := r.ReadFile(ctx, "a.txt"); !errors.Is(err, boom) {
		t.Fatalf("ReadFile error = %v, want backend error surfaced", err)
	}
	if _, err := r.Walk(ctx, "."); !errors.Is(err, boom) {
		t.Fatalf("Walk error = %v, want backend error surfaced", err)
	}
}

// Property: any set of confined files written through Remote is exactly what Walk
// lists and what ReadFile returns, so the port maps onto the transport without
// losing, duplicating, or corrupting a file.
func TestProp_RemoteFilesRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		seg := rapid.StringMatching(`[a-z]{1,4}`)
		pathGen := rapid.Custom(func(t *rapid.T) string {
			segs := rapid.SliceOfN(seg, 1, 3).Draw(t, "segs")
			return strings.Join(segs, "/")
		})
		n := rapid.IntRange(0, 12).Draw(rt, "n")

		want := map[string]string{}
		r := NewRemote(newFakeTransport())
		ctx := context.Background()
		for range n {
			p := pathGen.Draw(rt, "path")
			content := rapid.String().Draw(rt, "content")
			if err := r.WriteFile(ctx, p, []byte(content)); err != nil {
				rt.Fatalf("write %q: %v", p, err)
			}
			want[normPath(p)] = content // last write wins, mirroring the backend map
		}

		walk, err := r.Walk(ctx, ".")
		if err != nil {
			rt.Fatalf("walk: %v", err)
		}
		wantPaths := make([]string, 0, len(want))
		for p := range want {
			wantPaths = append(wantPaths, p)
		}
		sort.Strings(wantPaths)
		if !equalStrings(walk, wantPaths) {
			rt.Fatalf("walk = %v, want %v", walk, wantPaths)
		}
		for p, content := range want {
			got, err := r.ReadFile(ctx, p)
			if err != nil || string(got) != content {
				rt.Fatalf("read %q = %q, %v; want %q", p, got, err, content)
			}
		}
	})
}

// path normalises a generated path the way confine does, so the test's expected
// set matches what the backend stored.
func normPath(p string) string {
	c, err := confine(p)
	if err != nil {
		panic(err)
	}
	return c
}

// equalStrings compares two string slices element-wise, treating a nil and an
// empty slice as equal (the backend returns nil for an empty listing).
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
