package auth_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/integrations/auth"
)

// fakeExchanger is a scripted token endpoint: it records each request and returns a
// canned response, optionally varying it by call number so a refresh can be observed.
type fakeExchanger struct {
	mu      sync.Mutex
	calls   int
	gotAuth string
	gotBody string
	reply   func(call int) (int, string)
}

func (f *fakeExchanger) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotAuth = req.Header.Get("Authorization")
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.gotBody = string(b)
	}
	status, body := 200, `{"access_token":"at","expires_in":3600}`
	if f.reply != nil {
		status, body = f.reply(f.calls)
	}
	resp := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
	// Mirror the request transport's contract: a non-2xx status returns the response
	// alongside a classified fault (4xx terminal, 5xx transient).
	if status < 200 || status >= 300 {
		cls := fault.Terminal
		if status >= 500 {
			cls = fault.Transient
		}
		return resp, fault.New(cls, "http_status", "token endpoint status")
	}
	return resp, nil
}

func (f *fakeExchanger) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestOAuth2ClientCredentials(t *testing.T) {
	ex := &fakeExchanger{reply: func(int) (int, string) { return 200, `{"access_token":"at-1","expires_in":3600}` }}
	p, err := auth.FromConfig(auth.Config{
		Type:            auth.SchemeOAuth2,
		TokenURL:        "https://idp.example.com/token",
		ClientID:        "client-abc",
		ClientSecretRef: "CS",
		Scopes:          []string{"read", "write"},
	}, auth.WithTokenExchanger(ex), auth.WithClock(clock.NewManual(time.Unix(0, 0).UTC())))
	if err != nil {
		t.Fatalf("from config: %v", err)
	}

	req := newReq(t)
	if err := p.Apply(context.Background(), req, mapSource{"CS": "client-secret"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-1" {
		t.Fatalf("request auth header: %q", got)
	}
	// The token request used Basic client auth and the client_credentials grant.
	if !strings.HasPrefix(ex.gotAuth, "Basic ") {
		t.Fatalf("token request client auth: %q", ex.gotAuth)
	}
	if !strings.Contains(ex.gotBody, "grant_type=client_credentials") {
		t.Fatalf("token request body: %q", ex.gotBody)
	}
	if !strings.Contains(ex.gotBody, "scope=read+write") {
		t.Fatalf("scopes not sent: %q", ex.gotBody)
	}
}

func TestOAuth2CachesUntilExpiry(t *testing.T) {
	clk := clock.NewManual(time.Unix(0, 0).UTC())
	ex := &fakeExchanger{reply: func(call int) (int, string) {
		return 200, `{"access_token":"at-` + itoa(call) + `","expires_in":3600}`
	}}
	p, _ := auth.FromConfig(auth.Config{
		Type: auth.SchemeOAuth2, TokenURL: "https://idp/token", ClientID: "c",
	}, auth.WithTokenExchanger(ex), auth.WithClock(clk))

	src := mapSource{}
	// First two calls within the lifetime share one fetched token.
	for range 3 {
		req := newReq(t)
		if err := p.Apply(context.Background(), req, src); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if req.Header.Get("Authorization") != "Bearer at-1" {
			t.Fatalf("expected cached token at-1, got %q", req.Header.Get("Authorization"))
		}
	}
	if ex.callCount() != 1 {
		t.Fatalf("expected one token fetch, got %d", ex.callCount())
	}

	// After the token lapses, the next Apply refreshes.
	clk.Advance(2 * time.Hour)
	req := newReq(t)
	if err := p.Apply(context.Background(), req, src); err != nil {
		t.Fatalf("apply after expiry: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer at-2" {
		t.Fatalf("expected refreshed token at-2, got %q", req.Header.Get("Authorization"))
	}
	if ex.callCount() != 2 {
		t.Fatalf("expected a second token fetch, got %d", ex.callCount())
	}
}

func TestOAuth2RefreshTokenGrant(t *testing.T) {
	ex := &fakeExchanger{}
	p, err := auth.FromConfig(auth.Config{
		Type:            auth.SchemeOAuth2,
		TokenURL:        "https://idp/token",
		Grant:           auth.GrantRefreshToken,
		RefreshTokenRef: "RT",
	}, auth.WithTokenExchanger(ex), auth.WithClock(clock.NewManual(time.Unix(0, 0).UTC())))
	if err != nil {
		t.Fatalf("from config: %v", err)
	}
	if err := p.Apply(context.Background(), newReq(t), mapSource{"RT": "refresh-xyz"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(ex.gotBody, "grant_type=refresh_token") {
		t.Fatalf("grant: %q", ex.gotBody)
	}
	if !strings.Contains(ex.gotBody, "refresh_token=refresh-xyz") {
		t.Fatalf("refresh token not sent: %q", ex.gotBody)
	}
}

func TestOAuth2TokenEndpointError(t *testing.T) {
	ex := &fakeExchanger{reply: func(int) (int, string) { return 401, `{"error":"invalid_client"}` }}
	p, _ := auth.FromConfig(auth.Config{
		Type: auth.SchemeOAuth2, TokenURL: "https://idp/token", ClientID: "c",
	}, auth.WithTokenExchanger(ex), auth.WithClock(clock.NewManual(time.Unix(0, 0).UTC())))
	err := p.Apply(context.Background(), newReq(t), mapSource{})
	if err == nil || fault.Classify(err) != fault.Terminal {
		t.Fatalf("expected a terminal error on a 401 token response, got %v", err)
	}
}

func TestOAuth2NoAccessToken(t *testing.T) {
	ex := &fakeExchanger{reply: func(int) (int, string) { return 200, `{"token_type":"bearer"}` }}
	p, _ := auth.FromConfig(auth.Config{
		Type: auth.SchemeOAuth2, TokenURL: "https://idp/token", ClientID: "c",
	}, auth.WithTokenExchanger(ex), auth.WithClock(clock.NewManual(time.Unix(0, 0).UTC())))
	if err := p.Apply(context.Background(), newReq(t), mapSource{}); err == nil {
		t.Fatal("expected an error when the token response has no access_token")
	}
}

func TestOAuth2ConfigErrors(t *testing.T) {
	ex := auth.WithTokenExchanger(&fakeExchanger{})
	cases := []struct {
		name string
		cfg  auth.Config
		opts []auth.Option
	}{
		{"no token url", auth.Config{Type: auth.SchemeOAuth2, ClientID: "c"}, []auth.Option{ex}},
		{"client creds no client id", auth.Config{Type: auth.SchemeOAuth2, TokenURL: "u"}, []auth.Option{ex}},
		{"refresh no ref", auth.Config{Type: auth.SchemeOAuth2, TokenURL: "u", Grant: auth.GrantRefreshToken}, []auth.Option{ex}},
		{"unknown grant", auth.Config{Type: auth.SchemeOAuth2, TokenURL: "u", ClientID: "c", Grant: "implicit"}, []auth.Option{ex}},
		{"no exchanger", auth.Config{Type: auth.SchemeOAuth2, TokenURL: "u", ClientID: "c"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := auth.FromConfig(c.cfg, c.opts...); err == nil {
				t.Fatalf("expected a config error")
			}
		})
	}
}

// TestOAuth2ConcurrentApplyOneFetch asserts concurrent Apply calls trigger a single
// token fetch, the others reusing the cached token.
func TestOAuth2ConcurrentApplyOneFetch(t *testing.T) {
	ex := &fakeExchanger{}
	p, _ := auth.FromConfig(auth.Config{
		Type: auth.SchemeOAuth2, TokenURL: "https://idp/token", ClientID: "c",
	}, auth.WithTokenExchanger(ex), auth.WithClock(clock.NewManual(time.Unix(0, 0).UTC())))

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Apply(context.Background(), newReq(t), mapSource{})
		}()
	}
	wg.Wait()
	if ex.callCount() != 1 {
		t.Fatalf("expected exactly one token fetch under concurrency, got %d", ex.callCount())
	}
}

// TestOAuth2HugeExpiresInClamped proves an absurd expires_in does not overflow the
// expiry arithmetic into the past and cause a refetch on every call.
func TestOAuth2HugeExpiresInClamped(t *testing.T) {
	ex := &fakeExchanger{reply: func(call int) (int, string) {
		return 200, `{"access_token":"at-` + itoa(call) + `","expires_in":99999999999999999}`
	}}
	p, _ := auth.FromConfig(auth.Config{
		Type: auth.SchemeOAuth2, TokenURL: "https://idp/token", ClientID: "c",
	}, auth.WithTokenExchanger(ex), auth.WithClock(clock.NewManual(time.Unix(0, 0).UTC())))

	src := mapSource{}
	if err := p.Apply(context.Background(), newReq(t), src); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	req := newReq(t)
	if err := p.Apply(context.Background(), req, src); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if req.Header.Get("Authorization") != "Bearer at-1" {
		t.Fatalf("a huge expires_in must not force a refetch, got %q", req.Header.Get("Authorization"))
	}
	if ex.callCount() != 1 {
		t.Fatalf("expected the clamped token to be cached (one fetch), got %d", ex.callCount())
	}
}

func itoa(n int) string {
	return string(rune('0' + n))
}
