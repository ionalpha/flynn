package extension

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/netguard"
)

// HostFetcher lends network access to an extension that runs with egress
// denied. The fetcher owns the destination; nothing the extension sends picks
// a host, path or scheme, so a compromised extension can reach only the one
// endpoint the operator granted.
type HostFetcher interface {
	Fetch(ctx context.Context, body []byte) ([]byte, error)
}

// HTTPHostFetcher POSTs to an endpoint fixed at construction. The dial control
// re-checks the resolved address at connect time, so DNS rebinding to a
// private address is refused.
type HTTPHostFetcher struct {
	endpoint    string
	contentType string
	client      *http.Client
	maxResponse int64
}

// FetcherOption configures an HTTPHostFetcher.
type FetcherOption func(*httpFetcherConfig)

type httpFetcherConfig struct {
	policy      netguard.Policy
	contentType string
	timeout     time.Duration
	maxResponse int64
	private     bool
}

// WithFetchTimeout bounds a single request. The default is 30s.
func WithFetchTimeout(d time.Duration) FetcherOption {
	return func(c *httpFetcherConfig) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithMaxResponseBytes bounds the response body. The default is 1 MiB.
func WithMaxResponseBytes(n int64) FetcherOption {
	return func(c *httpFetcherConfig) {
		if n > 0 {
			c.maxResponse = n
		}
	}
}

// WithFetchContentType sets the request's Content-Type. The default is application/json.
func WithFetchContentType(s string) FetcherOption {
	return func(c *httpFetcherConfig) {
		if s != "" {
			c.contentType = s
		}
	}
}

// WithPrivateEndpoint permits a loopback or private endpoint, such as a local
// test validator. It lifts the anti-SSRF rule, so it is opt-in only.
func WithPrivateEndpoint() FetcherOption {
	return func(c *httpFetcherConfig) { c.private = true }
}

// NewHTTPHostFetcher refuses an endpoint that is not an absolute http(s) URL
// and, without [WithPrivateEndpoint], a literal private, loopback or
// link-local host.
func NewHTTPHostFetcher(endpoint string, opts ...FetcherOption) (*HTTPHostFetcher, error) {
	cfg := httpFetcherConfig{
		contentType: "application/json",
		timeout:     30 * time.Second,
		maxResponse: 1 << 20,
	}
	for _, o := range opts {
		o(&cfg)
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_fetch_endpoint", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fault.New(fault.Terminal, "extension_fetch_endpoint",
			"extension: fetch endpoint must be an http or https URL")
	}
	if u.Host == "" {
		return nil, fault.New(fault.Terminal, "extension_fetch_endpoint",
			"extension: fetch endpoint has no host")
	}

	// Public-only by default. WithPrivateEndpoint allows exactly the
	// endpoint's own literal address instead.
	cfg.policy.AllowHosts = []string{u.Hostname()}
	if cfg.private {
		addr, perr := hostPrefix(u.Hostname())
		if perr != nil {
			return nil, perr
		}
		cfg.policy.Allow = []netip.Prefix{addr}
	} else {
		cfg.policy.AllowPublic = true
		// Dial would deny it too; refusing at grant time tells the operator
		// before a mint is half done.
		if addr, err := netip.ParseAddr(u.Hostname()); err == nil && !netguard.IsPublic(addr) {
			return nil, fault.New(fault.Forbidden, "extension_fetch_endpoint",
				"extension: fetch endpoint "+u.Hostname()+" is not a public address; a local endpoint must be opted into explicitly")
		}
	}

	client := netguard.Client(cfg.policy)
	client.Timeout = cfg.timeout
	return &HTTPHostFetcher{
		endpoint:    endpoint,
		contentType: cfg.contentType,
		client:      client,
		maxResponse: cfg.maxResponse,
	}, nil
}

// hostPrefix requires a literal IP: resolving a name here would let a later
// DNS answer differ from the one checked.
func hostPrefix(host string) (netip.Prefix, error) {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Prefix{}, fault.New(fault.Terminal, "extension_fetch_endpoint",
			"extension: a private fetch endpoint must be a literal IP address, not a name")
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// Fetch POSTs body and returns the bounded response. A non-2xx error carries
// the status without the body, so an error page cannot reach the extension.
// Transport failures are transient.
func (f *HTTPHostFetcher) Fetch(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "extension_fetch_request", err)
	}
	req.Header.Set("Content-Type", f.contentType)

	res, err := f.client.Do(req)
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "extension_fetch_send", err)
	}
	defer func() { _ = res.Body.Close() }()

	// Read one byte past the cap, so an over-sized body is refused and never
	// truncated.
	out, err := io.ReadAll(io.LimitReader(res.Body, f.maxResponse+1))
	if err != nil {
		return nil, fault.Wrap(fault.Transient, "extension_fetch_read", err)
	}
	if int64(len(out)) > f.maxResponse {
		return nil, fault.New(fault.Forbidden, "extension_fetch_too_large",
			"extension: fetch response exceeded "+strconv.FormatInt(f.maxResponse, 10)+" bytes")
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fault.New(fault.Transient, "extension_fetch_status",
			"extension: fetch endpoint returned status "+strconv.Itoa(res.StatusCode))
	}
	return out, nil
}

var _ HostFetcher = (*HTTPHostFetcher)(nil)
