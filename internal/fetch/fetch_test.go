package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// serve starts an https test server returning body with status 200, and a client
// that trusts it and reaches 127.0.0.1 (bypassing the production SSRF dial guard,
// which is exercised separately).
func serve(t *testing.T, body []byte) (*httptest.Server, *Downloader) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, New(WithHTTPClient(srv.Client()))
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestFetchVerifiesAndInstalls(t *testing.T) {
	body := []byte("pretend these are model weights")
	srv, d := serve(t, body)
	dest := filepath.Join(t.TempDir(), "model.gguf")

	res, err := d.Fetch(context.Background(), Request{
		URL: srv.URL, Dest: dest, ExpectSHA256: sha(body), MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SHA256 != sha(body) || res.Bytes != int64(len(body)) || !res.Pinned {
		t.Fatalf("result wrong: %+v", res)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(body) {
		t.Fatalf("installed file wrong: %q err=%v", got, err)
	}
}

func TestFetchPinOnFetchWhenUnpinned(t *testing.T) {
	body := []byte("weights")
	srv, d := serve(t, body)
	res, err := d.Fetch(context.Background(), Request{URL: srv.URL, Dest: filepath.Join(t.TempDir(), "m")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pinned || res.SHA256 != sha(body) {
		t.Fatalf("unpinned fetch should compute but not claim a pin: %+v", res)
	}
}

func TestFetchRejectsDigestMismatch(t *testing.T) {
	srv, d := serve(t, []byte("the real bytes"))
	dest := filepath.Join(t.TempDir(), "m.gguf")
	_, err := d.Fetch(context.Background(), Request{
		URL: srv.URL, Dest: dest, ExpectSHA256: sha([]byte("different bytes")),
	})
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected digest mismatch, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a digest-mismatched download must not be installed")
	}
}

func TestFetchRejectsOversizeByHeader(t *testing.T) {
	srv, d := serve(t, make([]byte, 4096)) // Content-Length advertised
	dest := filepath.Join(t.TempDir(), "m")
	_, err := d.Fetch(context.Background(), Request{URL: srv.URL, Dest: dest, MaxBytes: 1024})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("expected oversize rejection, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("oversize download must not be installed")
	}
}

func TestFetchRejectsOversizeByStream(t *testing.T) {
	// Chunked response with no Content-Length, so the cap must be enforced on the
	// stream, not just the advertised header.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, _ := w.(http.Flusher)
		for range 8 {
			_, _ = w.Write(make([]byte, 512))
			if fl != nil {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	d := New(WithHTTPClient(srv.Client()))
	dest := filepath.Join(t.TempDir(), "m")
	_, err := d.Fetch(context.Background(), Request{URL: srv.URL, Dest: dest, MaxBytes: 1024})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("expected stream oversize rejection, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("stream-oversize download must not be installed")
	}
}

func TestFetchRefusesNonHTTPS(t *testing.T) {
	d := New(WithHTTPClient(http.DefaultClient))
	_, err := d.Fetch(context.Background(), Request{URL: "http://example.com/x", Dest: "x"})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("non-https should be refused, got %v", err)
	}
}

func TestFetchRejectsNon200(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	d := New(WithHTTPClient(srv.Client()))
	_, err := d.Fetch(context.Background(), Request{URL: srv.URL, Dest: filepath.Join(t.TempDir(), "m")})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected a 404 error, got %v", err)
	}
}

// TestDefaultClientBlocksPrivateAddress proves the default download client refuses
// to connect to a loopback address, the anti-SSRF policy. The server is on
// 127.0.0.1, which the policy must reject before any data is read.
func TestDefaultClientBlocksPrivateAddress(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should never be read"))
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "m")
	_, err := New().Fetch(context.Background(), Request{URL: srv.URL, Dest: dest})
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Fatalf("the default client must refuse a non-public address, got %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a blocked download must not be installed")
	}
}

// TestFetchProperties checks the verification contract over random payloads: with
// the right pinned digest the installed file is exactly what was served and the
// reported digest matches; with a wrong digest the download always errors and
// installs nothing.
func TestFetchProperties(t *testing.T) {
	var current []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(current)
	}))
	t.Cleanup(srv.Close)
	d := New(WithHTTPClient(srv.Client()))

	rapid.Check(t, func(rt *rapid.T) {
		current = []byte(rapid.StringN(0, 4096, -1).Draw(rt, "body"))
		dir := t.TempDir()

		good := filepath.Join(dir, "good")
		res, err := d.Fetch(context.Background(), Request{URL: srv.URL, Dest: good, ExpectSHA256: sha(current), MaxBytes: 1 << 20})
		if err != nil {
			rt.Fatalf("correct digest should verify: %v", err)
		}
		if res.SHA256 != sha(current) || res.Bytes != int64(len(current)) {
			rt.Fatalf("result mismatch: %+v vs %d bytes", res, len(current))
		}
		got, _ := os.ReadFile(good)
		if string(got) != string(current) {
			rt.Fatal("installed bytes differ from served bytes")
		}

		bad := filepath.Join(dir, "bad")
		if _, err := d.Fetch(context.Background(), Request{URL: srv.URL, Dest: bad, ExpectSHA256: sha(append(current, 'x')), MaxBytes: 1 << 20}); err == nil {
			rt.Fatal("a wrong digest must be rejected")
		}
		if _, statErr := os.Stat(bad); !os.IsNotExist(statErr) {
			rt.Fatal("a rejected download must not be installed")
		}
	})
}
