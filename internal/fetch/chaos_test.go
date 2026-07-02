package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// faultyBody yields data until failAfter bytes, then returns an injected error, the
// HTTP-stream analog of the testkit fault plans: a download that dies mid-flight.
type faultyBody struct {
	data      []byte
	pos       int
	failAfter int
}

func (b *faultyBody) Read(p []byte) (int, error) {
	if b.pos >= b.failAfter {
		return 0, errors.New("injected mid-stream network failure")
	}
	end := min(b.pos+len(p), min(b.failAfter, len(b.data)))
	n := copy(p, b.data[b.pos:end])
	b.pos += n
	if n == 0 {
		return 0, errors.New("injected mid-stream network failure")
	}
	return n, nil
}

func (b *faultyBody) Close() error { return nil }

// faultyTransport returns a 200 response whose body fails partway through.
type faultyTransport struct {
	data      []byte
	failAfter int
}

func (ft faultyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    200,
		Body:          &faultyBody{data: ft.data, failAfter: ft.failAfter},
		ContentLength: int64(len(ft.data)),
		Header:        make(http.Header),
		Request:       r,
	}, nil
}

// TestFetchMidStreamFailureInstallsNothing is the chaos invariant: when the download
// dies partway through, Fetch fails and leaves no file, never a truncated install.
func TestFetchMidStreamFailureInstallsNothing(t *testing.T) {
	full := make([]byte, 8192)
	for i := range full {
		full[i] = byte(i)
	}
	d := New(WithHTTPClient(&http.Client{Transport: faultyTransport{data: full, failAfter: 1000}}))
	dest := filepath.Join(t.TempDir(), "m.gguf")

	_, err := d.Fetch(context.Background(), Request{URL: "https://example.com/w", Dest: dest, ExpectSHA256: sha(full), MaxBytes: 1 << 20})
	if err == nil {
		t.Fatal("a mid-stream failure must error")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a failed download must not leave a file behind")
	}
}

// TestFetchTruncatedBodyInstallsNothing covers a server that advertises more bytes
// than it sends (a truncated or lying response): the short read must fail the
// download, not install a partial file.
func TestFetchTruncatedBodyInstallsNothing(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(100000)) // claim 100k
		_, _ = w.Write(make([]byte, 200))                      // send 200
	}))
	t.Cleanup(srv.Close)
	d := New(WithHTTPClient(srv.Client()))
	dest := filepath.Join(t.TempDir(), "m.gguf")

	_, err := d.Fetch(context.Background(), Request{URL: srv.URL, Dest: dest, MaxBytes: 1 << 20})
	if err == nil {
		t.Fatal("a truncated body must error")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a truncated download must not be installed")
	}
}

// TestFetchCancelledInstallsNothing covers a download cancelled mid-request:
// the server holds the request open, the client cancels, and Fetch must error
// with no file left. Cancellation is ordered explicitly: the context is
// cancelled only after the server has the request, and the handler aborts the
// connection rather than returning, so a response can never be produced and
// the cancel cannot lose a race against a completed download.
func TestFetchCancelledInstallsNothing(t *testing.T) {
	requested := make(chan struct{})
	var once sync.Once
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requested) })
		<-r.Context().Done() // hold the request open until the client cancels
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	d := New(WithHTTPClient(srv.Client()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-requested
		cancel()
	}()
	dest := filepath.Join(t.TempDir(), "m.gguf")

	_, err := d.Fetch(ctx, Request{URL: srv.URL, Dest: dest, MaxBytes: 1 << 20})
	if err == nil {
		t.Fatal("a cancelled download must error")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a cancelled download must not be installed")
	}
}
