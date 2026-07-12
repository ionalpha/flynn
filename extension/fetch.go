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

// HostFetcher sends a request body on behalf of a mounted extension tool and returns the
// response body. It is the network authority an extension borrows instead of holding: the
// extension process itself runs with egress fully denied, hands out opaque request bytes, and
// receives opaque response bytes back.
//
// The destination is the fetcher's, never the extension's. Nothing in the request the
// extension hands out selects a host, a path, or a scheme, so a compromised extension cannot
// point the host's network at an address of its choosing (no SSRF, no exfiltration to a
// third party): the worst it can do is send garbage to the one endpoint the operator already
// granted it.
type HostFetcher interface {
	// Fetch sends body to the fetcher's endpoint and returns the response body.
	Fetch(ctx context.Context, body []byte) ([]byte, error)
}

// HTTPHostFetcher is the default HostFetcher: it POSTs the request body to one fixed endpoint
// over a netguard-policed HTTP client, and returns the response body bounded.
//
// The endpoint is fixed at construction, so it is the operator's choice and not the
// extension's. The client's dial control re-checks the resolved address against the policy at
// connect time, so a DNS answer that swings to a private or loopback address after the name
// passed its check is still refused (anti-rebinding).
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

// WithMaxResponseBytes bounds the response body read from the endpoint, so an endpoint that
// answers with an unbounded stream cannot exhaust host memory. The default is 1 MiB.
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

// WithPrivateEndpoint permits an endpoint on a loopback or private address. It exists for a
// local service an operator deliberately runs (a test validator, a self-hosted node on the
// same box) and must be an explicit opt-in, because it turns off the anti-SSRF address rule
// that otherwise keeps a granted endpoint on the public internet. The default refuses a
// private endpoint, so a misconfiguration fails closed.
func WithPrivateEndpoint() FetcherOption {
	return func(c *httpFetcherConfig) { c.private = true }
}

// NewHTTPHostFetcher returns a fetcher that POSTs to endpoint. It refuses an endpoint that is
// not an absolute http/https URL, and (unless WithPrivateEndpoint is given) one whose host is
// a literal private, loopback, or link-local address, so a grant cannot silently aim the
// host's network at its own internals or the cloud metadata endpoint.
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

	// The address rules. Public-only is the anti-SSRF default: the name must resolve to a
	// globally-routable address, so a granted endpoint can never reach the host's own network
	// or the cloud metadata range. WithPrivateEndpoint swaps that for an allowlist of exactly
	// the endpoint's own literal address, which is the only way a loopback grant is honoured.
	cfg.policy.AllowHosts = []string{u.Hostname()}
	if cfg.private {
		addr, perr := hostPrefix(u.Hostname())
		if perr != nil {
			return nil, perr
		}
		cfg.policy.Allow = []netip.Prefix{addr}
	} else {
		cfg.policy.AllowPublic = true
		// A literal private address is refused here, at grant time, rather than left to fail at
		// the first send. The dial-time check would deny it either way, but an operator who
		// pointed an extension at loopback or the metadata endpoint should learn that when they
		// configure it, not when a mint is half-done.
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

// hostPrefix turns a private endpoint's host into the single-address prefix the policy allows.
// It requires a literal IP: a NAME that resolves privately is refused, because resolving it
// here would let a later answer differ from the one checked (the rebinding hole the dial-time
// check exists to close).
func hostPrefix(host string) (netip.Prefix, error) {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Prefix{}, fault.New(fault.Terminal, "extension_fetch_endpoint",
			"extension: a private fetch endpoint must be a literal IP address, not a name")
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// Fetch POSTs body to the fixed endpoint and returns the response body, bounded. A non-2xx
// status is returned as an error carrying the status but not the body, so an endpoint's error
// page cannot become a channel into the extension. A transport failure is transient: the
// caller delivers it to the tool, whose own failure path decides how to unwind.
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

	// Read one byte past the cap so a body exactly at the limit is not mistaken for an
	// over-sized one, and an over-sized one is refused rather than silently truncated into a
	// half-parsed response.
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
