package fetch

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// FuzzCheckURL throws arbitrary strings at the URL guard, which reads a value that
// comes from the catalog or a redirect (possibly hostile). Invariants: it never
// panics, and any URL it accepts is parseable, https, and has a host, so a request
// can never travel over a non-https scheme or to an empty host.
func FuzzCheckURL(f *testing.F) {
	for _, s := range []string{
		"", "https://example.com/a", "http://example.com", "ftp://x", "https://",
		"://", "https://[::1]/x", "HTTPS://X/Y", "https://h\x00st/", "\x00", "https:///path",
		"https://user:pass@host/x", "https://host:99999/x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if err := checkURL(raw); err == nil {
			u, perr := url.Parse(raw)
			if perr != nil || u.Scheme != "https" || u.Host == "" {
				t.Fatalf("checkURL accepted a non-https or hostless URL: %q", raw)
			}
		}
	})
}

// FuzzGuardDial throws arbitrary dial addresses at the anti-SSRF guard, which sees
// the address the client is about to connect to (after DNS resolution). Invariants:
// it never panics, and any address it allows resolves to a public IP, so localhost,
// a private network, link-local, or the cloud metadata endpoint can never be dialed.
func FuzzGuardDial(f *testing.F) {
	for _, s := range []string{
		"1.2.3.4:443", "127.0.0.1:80", "[::1]:443", "169.254.169.254:80", "10.0.0.1:1",
		"8.8.8.8:53", "[fd00::1]:443", "garbage", "host:port", ":443", "", "1.2.3.4",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, addr string) {
		if err := guardDial("tcp", addr, nil); err == nil {
			host, _, splitErr := net.SplitHostPort(addr)
			if splitErr != nil || !isPublicIP(net.ParseIP(host)) {
				t.Fatalf("guardDial allowed a non-public address: %q", addr)
			}
		}
	})
}

// FuzzFetch is the end-to-end security invariant under arbitrary server responses:
// whatever the body, status, and pinned digest, Fetch either succeeds with the file
// installed equal to the served bytes and the pinned digest matched, or it fails and
// installs nothing. There is never a partial, corrupt, or wrong-digest file on disk.
func FuzzFetch(f *testing.F) {
	var mu sync.Mutex
	var body []byte
	var status int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		b, s := append([]byte(nil), body...), status
		mu.Unlock()
		if s != http.StatusOK {
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	d := New(WithHTTPClient(srv.Client()))

	f.Add([]byte("weights"), 200, false)
	f.Add([]byte(""), 200, false)
	f.Add([]byte("x"), 200, true)
	f.Add([]byte("y"), 404, false)
	f.Add([]byte("z"), 500, true)

	f.Fuzz(func(t *testing.T, content []byte, st int, wrongDigest bool) {
		if st < 200 || st > 599 {
			st = 200 // keep the server out of invalid/informational status panics
		}
		mu.Lock()
		body, status = content, st
		mu.Unlock()

		dest := filepath.Join(t.TempDir(), "m")
		digest := sha(content)
		if wrongDigest {
			digest = sha(append(append([]byte(nil), content...), 0))
		}
		res, err := d.Fetch(context.Background(), Request{
			URL: srv.URL, Dest: dest, ExpectSHA256: digest, MaxBytes: 1 << 20,
		})
		_, statErr := os.Stat(dest)
		installed := statErr == nil

		if err != nil {
			if installed {
				t.Fatalf("Fetch errored but installed a file (status=%d wrongDigest=%v)", st, wrongDigest)
			}
			return
		}
		// Success path: only legitimate on status 200 with the right digest, and the
		// installed file must be exactly the served bytes.
		if st != http.StatusOK || wrongDigest {
			t.Fatalf("Fetch succeeded on status=%d wrongDigest=%v", st, wrongDigest)
		}
		if !installed {
			t.Fatal("Fetch reported success but installed no file")
		}
		got, _ := os.ReadFile(dest)
		if !bytes.Equal(got, content) {
			t.Fatal("installed file differs from served bytes")
		}
		if res.SHA256 != sha(content) {
			t.Fatalf("result digest %s does not match content", res.SHA256)
		}
	})
}
