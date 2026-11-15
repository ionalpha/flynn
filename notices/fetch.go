package notices

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/netguard"
)

// FetchTimeout bounds the request.
const FetchTimeout = 5 * time.Second

// Source is where the signed feed is fetched from. The request is a plain GET with no
// query, cookies, authorization or client id, and a User-Agent of "flynn" with no
// version, so the origin cannot serve one release a different document. The origin
// still learns the IP address, which is why [OffEnv] exists.
type Source struct {
	// URL must be https.
	URL string
	// HTTP defaults to [SafeClient].
	HTTP *http.Client
}

// SafeClient dials public addresses only, through netguard like all other egress, and
// refuses redirects, which a static feed never needs.
func SafeClient() *http.Client {
	c := netguard.Client(netguard.PublicOnly())
	c.Timeout = FetchTimeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("notices: the feed endpoint does not redirect")
	}
	return c
}

// Fetch returns the feed document, reading at most [MaxFeedBytes]. It verifies
// nothing; [Accept] does.
func (s Source) Fetch(ctx context.Context) ([]byte, error) {
	if !strings.HasPrefix(s.URL, "https://") {
		return nil, fault.New(fault.Terminal, CodeFetch, "notices: the feed URL must be https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, http.NoBody)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeFetch, err)
	}
	// The only header set: nothing identifies the install, user or release.
	req.Header.Set("User-Agent", "flynn")

	client := s.HTTP
	if client == nil {
		client = SafeClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transient: the caller shows the cached feed and retries next run.
		return nil, fault.Wrap(fault.Transient, CodeFetch, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fault.New(fault.Transient, CodeFetch, "notices: feed endpoint returned "+resp.Status)
	}
	// Read one byte over the limit so an oversized body is reported as too large
	// instead of failing to parse.
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxFeedBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.Transient, CodeFetch, err)
	}
	if len(b) > MaxFeedBytes {
		return nil, fault.New(fault.Terminal, CodeTooLarge, "notices: feed document is too large")
	}
	// A shape check for diagnosis only. An origin without the feed deployed often
	// answers 200 with a web page, which would otherwise be reported as a bad
	// signature.
	if !isCOSE(b) {
		return nil, fault.New(fault.Transient, CodeFetch,
			"notices: the feed endpoint did not return a signed feed (is it deployed?)")
	}
	return b, nil
}

// isCOSE reports whether b starts with CBOR tag 18 and a four-element array, the
// prefix of a tagged COSE_Sign1 message.
func isCOSE(b []byte) bool {
	return len(b) >= 2 && b[0] == 0xd2 && b[1] == 0x84
}

// Refresh fetches, accepts and caches the feed. Nothing is written unless it verified,
// so a hostile origin cannot evict a good cached feed.
func Refresh(ctx context.Context, src Source, ring *Keyring, store *Store, now time.Time) (Feed, error) {
	doc, err := src.Fetch(ctx)
	if err != nil {
		return Feed{}, err
	}
	tr, err := store.LoadTrust()
	if err != nil {
		return Feed{}, err
	}
	f, tr, err := Accept(doc, ring, tr, now)
	if err != nil {
		return Feed{}, err
	}
	if err := store.SaveFeed(doc); err != nil {
		return Feed{}, err
	}
	if err := store.SaveTrust(tr); err != nil {
		return Feed{}, err
	}
	return f, nil
}
