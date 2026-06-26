// Package fetch downloads a file from an untrusted source and verifies it before it
// is installed: the generic, security-first transport for pulling any file (model
// weights, a plugin, a dataset) onto the local machine. It is download-and-verify
// ONLY: it writes a verified file to disk and never parses, loads, or executes it.
// What to do with the file, and any content policy, is the caller's concern; this
// package guarantees that the bytes on disk are exactly what was asked for, or that
// nothing is written at all.
//
// It is distinct from the integration request transport, which makes API calls:
// this one streams a large body, caps and hashes it as it arrives, verifies a
// digest, and installs atomically.
//
// Every layer is defensive. The transport refuses anything but https and refuses to
// connect to a non-public address (anti-SSRF, re-checked on every redirect hop so
// DNS rebinding cannot slip through, with no environment proxy to route around it).
// The download is capped on the stream so a hostile server cannot exhaust the disk,
// hashed as it is written, and verified against a caller-pinned digest so a
// compromised source cannot substitute a file. The install is atomic, so a partial,
// oversized, or mismatched download never appears as a usable file.
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
	"github.com/ionalpha/flynn/netguard"
)

// defaultMaxBytes is the fallback ceiling on a download when the caller does not
// give one, so an unbounded body can never fill the disk. Real callers pass the
// quantization's known size plus a small margin.
const defaultMaxBytes = 100 << 30 // 100 GiB

// Request describes one verified download.
type Request struct {
	// URL is the https source of the weights.
	URL string
	// Dest is the final install path; the file appears here only after it verifies.
	Dest string
	// ExpectSHA256 is the pinned digest the download is checked against (an optional
	// "sha256:" prefix is accepted). Empty means pin-on-fetch: the computed digest is
	// returned but nothing pre-pinned was verified, which is lower trust.
	ExpectSHA256 string
	// MaxBytes is the hard cap on the download size; 0 uses a safe default ceiling.
	MaxBytes int64
}

// Result reports a completed, verified download.
type Result struct {
	Path   string
	Bytes  int64
	SHA256 string
	// Pinned is true when the bytes were verified against a caller-pinned digest, the
	// strong-trust case; false when the digest was only computed (pin-on-fetch).
	Pinned bool
}

// Downloader performs verified downloads over a hardened HTTP client.
type Downloader struct{ http *http.Client }

// Option configures a Downloader.
type Option func(*Downloader)

// WithHTTPClient injects the HTTP client, so a test can supply one that reaches a
// local server. Production uses the default hardened client (see SafeClient).
func WithHTTPClient(c *http.Client) Option {
	return func(d *Downloader) {
		if c != nil {
			d.http = c
		}
	}
}

// New builds a Downloader. With no client injected it uses SafeClient, the
// anti-SSRF, https-only transport.
func New(opts ...Option) *Downloader {
	d := &Downloader{}
	for _, o := range opts {
		o(d)
	}
	if d.http == nil {
		// A download may reach any public source but never a private, loopback, or
		// metadata address; the policy enforces that at the point of connect.
		d.http = netguard.Client(netguard.PublicOnly())
	}
	return d
}

// Fetch downloads req.URL, verifies it, and installs it atomically at req.Dest,
// returning what was verified. It never executes the file. Any failure leaves no
// file at Dest: a partial, oversized, or digest-mismatched download is discarded.
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
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
	tmp, err := os.CreateTemp(filepath.Dir(req.Dest), ".fetch-*.part")
	if err != nil {
		return Result{}, fault.Wrap(fault.Terminal, "fetch_temp", err)
	}
	tmpName := tmp.Name()
	// Discard the partial file on any failure, so a bad download never installs.
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	// Read one byte past the cap so an over-cap body is detected rather than silently
	// truncated to exactly the cap.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return Result{}, fault.Wrap(fault.Transient, "fetch_copy", err)
	}
	if n > maxBytes {
		return Result{}, tooLarge(n, maxBytes)
	}
	if err := tmp.Sync(); err != nil {
		return Result{}, fault.Wrap(fault.Terminal, "fetch_sync", err)
	}
	if err := tmp.Close(); err != nil {
		return Result{}, fault.Wrap(fault.Terminal, "fetch_close", err)
	}

	sum := hex.EncodeToString(h.Sum(nil))
	want := strings.TrimPrefix(strings.ToLower(req.ExpectSHA256), "sha256:")
	if want != "" && !strings.EqualFold(sum, want) {
		return Result{}, fault.New(fault.Terminal, "fetch_digest",
			fmt.Sprintf("fetch: digest mismatch: want %s, got %s (download rejected)", want, sum))
	}
	if err := os.Rename(tmpName, req.Dest); err != nil {
		return Result{}, fault.Wrap(fault.Terminal, "fetch_install", err)
	}
	committed = true
	return Result{Path: req.Dest, Bytes: n, SHA256: sum, Pinned: want != ""}, nil
}

// checkURL requires an https URL with a host; the scheme guard pairs with the dial
// guard so a credential or a request never travels in the clear.
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

// tooLarge builds the oversize error, the disk-exhaustion guard's verdict.
func tooLarge(got, limit int64) error {
	return fault.New(fault.Terminal, "fetch_too_large",
		fmt.Sprintf("fetch: download exceeds the %d-byte cap (saw at least %d); rejected", limit, got))
}
