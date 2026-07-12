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

// FetchTimeout bounds the request. The notice check is never allowed to make a command
// slower than it would have been, so this is short and the caller runs it off the hot
// path entirely.
const FetchTimeout = 5 * time.Second

// Source is where the signed feed is fetched from.
//
// Every property of this request is chosen so that the origin learns as close to nothing
// as an HTTPS request permits. It is a plain unconditional GET for one fixed path with no
// query string, no cookies, no authorization, no client-generated id, and a user agent
// that names the product and nothing else, deliberately not the version: a version in the
// user agent would let an origin serve a different document to the users of one release,
// and being unable to distinguish clients is the entire point. The response is a static
// document, identical for everyone.
//
// The one thing an origin does learn is an IP address, which is unavoidable for anything
// that fetches over a network at all, and which is why the whole channel has an off
// switch.
type Source struct {
	// URL is the https address of the signed feed.
	URL string
	// HTTP is the client to use. Zero value uses SafeClient.
	HTTP *http.Client
}

// SafeClient is the transport the fetch uses: netguard's public-only dialer, so the
// notice fetch is governed by the same egress waist as everything else in the process and
// cannot be pointed at a private address, plus a redirect refusal. A static signed
// document has no reason to redirect, and following one is how a fetch ends up somewhere
// nobody audited.
func SafeClient() *http.Client {
	c := netguard.Client(netguard.PublicOnly())
	c.Timeout = FetchTimeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("notices: the feed endpoint does not redirect")
	}
	return c
}

// Fetch retrieves the signed feed document. It reads at most MaxFeedBytes, so an origin
// that starts streaming an endless body wastes its own time and not the client's memory.
// It does not verify anything: verification is Accept's job, and keeping the two apart
// means no path exists where bytes off a socket get used without going through it.
func (s Source) Fetch(ctx context.Context) ([]byte, error) {
	if !strings.HasPrefix(s.URL, "https://") {
		return nil, fault.New(fault.Terminal, CodeFetch, "notices: the feed URL must be https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, CodeFetch, err)
	}
	// Named, unversioned, and that is the whole header set. Nothing here identifies this
	// installation, this user, or this release.
	req.Header.Set("User-Agent", "flynn")

	client := s.HTTP
	if client == nil {
		client = SafeClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		// A failed notice check is a transient network condition far more often than it
		// is an attack, so it is Transient: the caller shows the cached feed and tries
		// again next run rather than failing the user's command.
		return nil, fault.Wrap(fault.Transient, CodeFetch, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fault.New(fault.Transient, CodeFetch, "notices: feed endpoint returned "+resp.Status)
	}
	// One byte over the ceiling is read on purpose, so an oversized body is detected as
	// oversized rather than silently truncated into a document that fails to parse for a
	// reason nobody can explain.
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxFeedBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.Transient, CodeFetch, err)
	}
	if len(b) > MaxFeedBytes {
		return nil, fault.New(fault.Terminal, CodeTooLarge, "notices: feed document is too large")
	}
	// A shape check, not a security check: verification is still the only thing that decides
	// whether these bytes are believed. It exists because an origin that has not had the
	// feed deployed to it yet tends to answer with a 200 and a web page rather than a 404,
	// and reporting that as a bad signature would send an operator hunting a cryptographic
	// bug when the real problem is a missing file.
	if !isCOSE(b) {
		return nil, fault.New(fault.Transient, CodeFetch,
			"notices: the feed endpoint did not return a signed feed (is it deployed?)")
	}
	return b, nil
}

// isCOSE reports whether b begins with a tagged COSE_Sign1 message (CBOR tag 18 followed by
// a four-element array), which is the only shape a feed document ever has.
func isCOSE(b []byte) bool {
	return len(b) >= 2 && b[0] == 0xd2 && b[1] == 0x84
}

// Refresh fetches the feed, accepts it against the keyring and the stored trust state,
// and caches it for the next run. It returns the accepted feed.
//
// Nothing is written unless the document verified, so a hostile or broken origin cannot
// even evict a good cached feed: the worst it can do is fail, leaving the last feed we
// did trust exactly where it was.
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
