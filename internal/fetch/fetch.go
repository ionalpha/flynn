// Package fetch downloads a file from an untrusted source (model weights, a plugin,
// a dataset) and verifies it before installing. It never parses, loads or runs the
// file, and guarantees the bytes on disk are what was asked for or that nothing is
// written. Content policy is the caller's. The integration request transport makes
// API calls; this one streams a large body, caps and hashes it as it arrives,
// verifies a digest and installs atomically.
//
// The transport allows only https and refuses non-public addresses, re-checked on
// every redirect so DNS rebinding cannot pass, with no environment proxy. The
// stream is capped so a hostile server cannot fill the disk, and the digest is
// checked against a caller pin so a compromised source cannot substitute a file.
// A partial, oversized or mismatched download never appears as a usable file.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fsatomic"
	"github.com/ionalpha/flynn/netguard"
)

// defaultMaxBytes caps a download when the caller gives no limit, so an unbounded
// body cannot fill the disk. Callers pass the known size plus a margin.
const defaultMaxBytes = 100 << 30 // 100 GiB

// Request describes one verified download.
type Request struct {
	// URL is the https source.
	URL string
	// Dest is the install path; the file appears there only after it verifies.
	Dest string
	// ExpectSHA256 is the pinned digest; a "sha256:" prefix is accepted. Empty is
	// pin-on-fetch: the computed digest is returned unverified, a lower trust.
	ExpectSHA256 string
	// MaxBytes caps the download size; 0 uses defaultMaxBytes.
	MaxBytes int64
}

// Result reports a completed, verified download.
type Result struct {
	Path   string
	Bytes  int64
	SHA256 string
	// Pinned is true when the bytes matched a caller-pinned digest and false on
	// pin-on-fetch.
	Pinned bool
}

// Downloader performs verified downloads over a hardened HTTP client.
type Downloader struct{ http *http.Client }

// Option configures a Downloader.
type Option func(*Downloader)

// WithHTTPClient injects the HTTP client, so a test can reach a local server.
func WithHTTPClient(c *http.Client) Option {
	return func(d *Downloader) {
		if c != nil {
			d.http = c
		}
	}
}

// New builds a Downloader. Without an injected client it uses the https-only,
// public-only netguard client.
func New(opts ...Option) *Downloader {
	d := &Downloader{}
	for _, o := range opts {
		o(d)
	}
	if d.http == nil {
		// A download may reach any public source and never a private, loopback or
		// metadata address; netguard enforces it at connect.
		d.http = netguard.Client(netguard.PublicOnly())
	}
	return d
}

// Fetch downloads req.URL, verifies it and installs it atomically at req.Dest. It
// never runs the file. A failure leaves no file at Dest.
func (d *Downloader) Fetch(ctx context.Context, req Request) (Result, error) {
	if err := checkURL(req.URL); err != nil {
		return Result{}, err
	}
	if req.Dest == "" {
		return Result{}, fault.New(fault.Terminal, "fetch_dest", "fetch: no destination path")
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, http.NoBody)
	if err != nil {
		return Result{}, fault.Wrap(fault.Terminal, "fetch_request", err)
	}
	resp, err := d.http.Do(httpReq)
	if err != nil {
		return Result{}, fault.Wrap(fault.Transient, "fetch_http", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fault.New(fault.Terminal, "fetch_status",
			fmt.Sprintf("fetch: HTTP %d from %s", resp.StatusCode, req.URL))
	}
	if resp.ContentLength > maxBytes {
		return Result{}, tooLarge(resp.ContentLength, maxBytes)
	}

	if err := os.MkdirAll(filepath.Dir(req.Dest), 0o750); err != nil {
		return Result{}, fault.Wrap(fault.Terminal, "fetch_mkdir", err)
	}
	var (
		n    int64
		sum  string
		want = strings.TrimPrefix(strings.ToLower(req.ExpectSHA256), "sha256:")
	)
	// The body streams into a sibling temp file and reaches Dest only whole and
	// digest-matched, because every rejection runs inside the producer: an error
	// abandons the temp file and leaves Dest untouched. A present Dest is always
	// fully verified.
	err = fsatomic.WriteStream(req.Dest, 0o600, func(w io.Writer) error {
		h := sha256.New()
		// Read one byte past the cap, so an over-cap body is detected rather than
		// truncated to the cap.
		var copyErr error
		n, copyErr = io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, maxBytes+1))
		if copyErr != nil {
			return fault.Wrap(fault.Transient, "fetch_copy", copyErr)
		}
		if n > maxBytes {
			return tooLarge(n, maxBytes)
		}
		sum = hex.EncodeToString(h.Sum(nil))
		if want != "" && !strings.EqualFold(sum, want) {
			return fault.New(fault.Terminal, "fetch_digest",
				fmt.Sprintf("fetch: digest mismatch: want %s, got %s (download rejected)", want, sum))
		}
		return nil
	})
	if err != nil {
		// A producer rejection already names its rule; only a write failure needs a
		// code.
		if fault.CodeOf(err) == "" {
			return Result{}, fault.Wrap(fault.Terminal, "fetch_install", err)
		}
		return Result{}, err
	}
	return Result{Path: req.Dest, Bytes: n, SHA256: sum, Pinned: want != ""}, nil
}

// checkURL requires an https URL with a host. With the dial guard it keeps
// credentials and requests off cleartext.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fault.New(fault.Terminal, "fetch_url", "fetch: invalid URL")
	}
	if u.Scheme != "https" {
		return fault.New(fault.Forbidden, "fetch_url_scheme", "fetch: weights must be fetched over https, got "+u.Scheme)
	}
	return nil
}

func tooLarge(got, limit int64) error {
	return fault.New(fault.Terminal, "fetch_too_large",
		fmt.Sprintf("fetch: download exceeds the %d-byte cap (saw at least %d); rejected", limit, got))
}
