// Package fetch downloads a model's weights and verifies them before they are
// installed, the security boundary between an untrusted registry and the local
// machine. It is download-and-verify ONLY: it never loads, parses, or runs the
// file. Running an untrusted model is a separate, sandboxed step, because a model
// file is hostile input to a known-vulnerable parser; the worst a corrupt or
// malicious download can do here is fail a check and be discarded.
//
// Every layer is defensive. The transport refuses anything but https and refuses
// to connect to a non-public address (anti-SSRF, re-checked on every redirect hop
// so DNS rebinding cannot slip through). The download is capped on the stream so a
// hostile server cannot exhaust the disk, hashed as it is written, and verified
// against a caller-pinned digest so a compromised registry cannot substitute a
// file. The install is atomic, so a partial or failed download never appears as a
// usable model. A code-executing weight format is refused outright.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ionalpha/flynn/fault"
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
	// Format is the weight format; a code-executing format (pickle and kin) is refused.
	Format string
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
		d.http = SafeClient()
	}
	return d
}

// SafeClient is the hardened HTTP client downloads use by default. It refuses to
// dial a non-public address (so a URL or a redirect cannot reach localhost, a
// private network, or the cloud metadata endpoint), refuses a non-https redirect,
// bounds redirects, and does not honor a proxy from the environment so the dial
// guard is authoritative. The dial guard runs after DNS resolution on the actual
// address, so a name that resolves to a private IP, including a rebinding attack,
// is still blocked.
func SafeClient() *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, Control: guardDial}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fault.New(fault.Terminal, "fetch_redirects", "fetch: too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fault.New(fault.Forbidden, "fetch_redirect_scheme", "fetch: refusing a non-https redirect to "+req.URL.Host)
			}
			return nil
		},
	}
}

// guardDial rejects a connection to any non-public IP, the anti-SSRF check at the
// point of connect.
func guardDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fault.New(fault.Forbidden, "fetch_addr", "fetch: cannot parse dial address "+address)
	}
	if ip := net.ParseIP(host); !isPublicIP(ip) {
		return fault.New(fault.Forbidden, "fetch_ssrf", "fetch: refusing to connect to non-public address "+host)
	}
	return nil
}

// isPublicIP reports whether ip is a routable public address, rejecting loopback,
// private (RFC1918 and IPv6 unique-local), link-local (which covers the
// 169.254.169.254 cloud metadata endpoint), multicast, and the unspecified address.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsUnspecified(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return false
	default:
		return true
	}
}

// Fetch downloads req.URL, verifies it, and installs it atomically at req.Dest,
// returning what was verified. It never executes the file. Any failure leaves no
// file at Dest: a partial, oversized, or digest-mismatched download is discarded.
func (d *Downloader) Fetch(ctx context.Context, req Request) (Result, error) {
	if isCodeExecFormat(req.Format) {
		return Result{}, fault.New(fault.Forbidden, "fetch_format",
			"fetch: refusing a code-executing weight format ("+req.Format+"); only tensor formats are fetched")
	}
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

// isCodeExecFormat reports whether a weight format can execute code when loaded
// (pickle and its file extensions), which is never fetched.
func isCodeExecFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "pickle", "pt", "pth", "bin":
		return true
	default:
		return false
	}
}

// tooLarge builds the oversize error, the disk-exhaustion guard's verdict.
func tooLarge(got, limit int64) error {
	return fault.New(fault.Terminal, "fetch_too_large",
		fmt.Sprintf("fetch: download exceeds the %d-byte cap (saw at least %d); rejected", limit, got))
}
