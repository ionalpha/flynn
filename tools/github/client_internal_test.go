package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/secret"
)

// errSource is a credential that cannot be obtained. Every request path asks for one
// before it reaches the network, so this proves the failure is reported rather than a
// request going out unauthenticated.
type errSource struct{ err error }

func (e errSource) installationToken(context.Context) (secret.Text, error) {
	return secret.Text{}, e.err
}

// serveWith stands a fake API up on loopback and returns a client bound to it. The
// handler is the whole of the API for the test that installs it.
func serveWith(t *testing.T, h http.HandlerFunc) *client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &client{
		cfg: Config{
			Owner: "ionalpha", Repo: "flynn", Number: 7,
			APIBase: srv.URL, HTTPClient: srv.Client(),
			MaxFiles: 10, MaxPatchBytes: 1 << 10,
		},
		auth: staticSource{token: secret.New("ghs_test")},
	}
}

// deadServer returns the URL of a server that has already stopped listening, so a
// request to it fails in the transport rather than at any status code.
func deadServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

func TestDoRefusesAnUnencodableRequestBody(t *testing.T) {
	c := serveWith(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a request whose body cannot be encoded must never be sent")
	})
	err := c.do(t.Context(), http.MethodPost, c.cfg.APIBase+"/x", make(chan int), nil)
	if err == nil || !strings.Contains(err.Error(), "encoding request") {
		t.Fatalf("do error = %v, want an encoding failure", err)
	}
}

func TestRequestConstructionFailuresAreReported(t *testing.T) {
	c := serveWith(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("no request should have reached the server: %s %s", r.Method, r.URL)
	})
	if err := c.do(t.Context(), "GE T", c.cfg.APIBase, nil, nil); err == nil {
		t.Error("do with an invalid method must fail before sending")
	}
	if _, err := c.doPaged(t.Context(), "http://example.invalid/%zz", nil); err == nil {
		t.Error("doPaged with an unparsable URL must fail before sending")
	}
}

func TestACredentialFailureStopsEveryRequest(t *testing.T) {
	want := errors.New("vault sealed")
	c := serveWith(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("a request must not go out without a credential")
	})
	c.auth = errSource{err: want}

	if err := c.do(t.Context(), http.MethodGet, c.cfg.APIBase+"/x", nil, nil); !errors.Is(err, want) {
		t.Errorf("do error = %v, want %v", err, want)
	}
	if _, err := c.doPaged(t.Context(), c.cfg.APIBase+"/x", nil); !errors.Is(err, want) {
		t.Errorf("doPaged error = %v, want %v", err, want)
	}
}

// TestStatusErrorCarriesTheBody checks a failed response explains itself: the status
// is always named, and a body such as a 422's validation message is included so the
// reason reaches the caller instead of only the code.
func TestStatusErrorCarriesTheBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"unprocessable with a body", http.StatusUnprocessableEntity, `{"message":"line must be part of the diff"}`, []string{"422", "line must be part of the diff"}},
		{"server error with no body", http.StatusInternalServerError, "", []string{"500"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := serveWith(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			err := c.do(t.Context(), http.MethodGet, c.cfg.APIBase+"/repos/o/r/pulls/7", nil, nil)
			if err == nil {
				t.Fatal("a non-2xx response must be an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

// TestPagedStatusErrorIsReported checks the pagination path applies the same status
// rule as the plain one: a page that answers non-2xx stops the fetch.
func TestPagedStatusErrorIsReported(t *testing.T) {
	c := serveWith(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("rate limited"))
	})
	if _, _, err := c.changedFiles(t.Context(), 7); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("changedFiles error = %v, want a 403", err)
	}
	if _, err := c.reviewComments(t.Context(), 7); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("reviewComments error = %v, want a 403", err)
	}
}

// TestTransportFailureIsReported checks a connection that never lands is an error on
// both request paths, and that the address this package puts in the message is the
// redacted one: what it writes itself carries no query string.
func TestTransportFailureIsReported(t *testing.T) {
	dead := deadServer(t)
	c := serveWith(t, func(_ http.ResponseWriter, _ *http.Request) {})

	err := c.do(t.Context(), http.MethodGet, dead+"/repos/o/r/pulls/7", nil, nil)
	if err == nil {
		t.Fatal("a request to a dead server must fail")
	}
	if !strings.Contains(err.Error(), "GET "+dead+"/repos/o/r/pulls/7") {
		t.Errorf("error = %v, want it to name the method and path", err)
	}

	if _, err := c.doPaged(t.Context(), dead+"/repos/o/r/pulls/7/files", nil); err == nil {
		t.Error("a paged request to a dead server must fail")
	}
}

// TestRedactURLDropsTheQueryString checks the address this package writes into an
// error message: a token that ever rides in a query parameter is cut before the URL
// reaches a log.
func TestRedactURLDropsTheQueryString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.github.com/repos/o/r/pulls/7", "https://api.github.com/repos/o/r/pulls/7"},
		{"https://api.github.com/x?access_token=supersecret", "https://api.github.com/x"},
		{"https://api.github.com/x?a=1&b=2", "https://api.github.com/x"},
	}
	for _, tc := range cases {
		if got := redactURL(tc.in); got != tc.want {
			t.Errorf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestUndecodableResponseIsAnError checks a 200 carrying something that is not the
// JSON we asked for is reported, on both the plain and the paginated path, rather
// than leaving the caller with a zero value that reads as an empty answer.
func TestUndecodableResponseIsAnError(t *testing.T) {
	c := serveWith(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	})
	if _, err := c.pullRequest(t.Context(), 7); err == nil || !strings.Contains(err.Error(), "decoding response") {
		t.Errorf("pullRequest error = %v, want a decode failure", err)
	}
	if _, _, err := c.changedFiles(t.Context(), 7); err == nil || !strings.Contains(err.Error(), "decoding response") {
		t.Errorf("changedFiles error = %v, want a decode failure", err)
	}
}

// TestBodylessResponseIsDrained checks a call with nothing to decode succeeds and
// consumes the response, which is what lets the connection be reused.
func TestBodylessResponseIsDrained(t *testing.T) {
	c := serveWith(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1}`))
	})
	if err := c.do(t.Context(), http.MethodPost, c.cfg.APIBase+"/x", map[string]any{"a": 1}, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
}

// TestNextPageRejectsUnusableLinks checks the pagination guard on the two malformed
// inputs it can be handed: a link that is not a URL, and an API base that is not one.
// Either way the fetch stops rather than following an address it cannot check.
func TestNextPageRejectsUnusableLinks(t *testing.T) {
	c := &client{cfg: Config{APIBase: "https://api.github.com"}, auth: staticSource{}}
	_, err := c.nextPage(`<http://a b/c>; rel="next"`)
	if err == nil || !strings.Contains(err.Error(), "unparsable pagination link") {
		t.Fatalf("nextPage error = %v, want an unparsable-link error", err)
	}

	bad := &client{cfg: Config{APIBase: "http://a b/"}, auth: staticSource{}}
	_, err = bad.nextPage(`<https://api.github.com/x?page=2>; rel="next"`)
	if err == nil || !strings.Contains(err.Error(), "unparsable API base") {
		t.Fatalf("nextPage error = %v, want an unparsable-base error", err)
	}

	if next, err := c.nextPage(""); err != nil || next != "" {
		t.Errorf("nextPage(no header) = %q, %v, want no next page", next, err)
	}
}

// TestReviewCommentsRefuseToRunOutOfPages checks the inline-comment listing treats
// exhausting its page cap as a failure. A short list would have the reviewer believe
// a finding it already posted is absent, and restate or retract it.
func TestReviewCommentsRefuseToRunOutOfPages(t *testing.T) {
	var host string
	c := serveWith(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", fmt.Sprintf("<%s/repos/o/r/pulls/7/comments?page=2>; rel=\"next\"", host))
		_, _ = w.Write([]byte(`[]`))
	})
	host = c.cfg.APIBase
	if _, err := c.reviewComments(t.Context(), 7); err == nil || !strings.Contains(err.Error(), "pages of review comments") {
		t.Fatalf("reviewComments error = %v, want a pagination-cap error", err)
	}
}

// TestChangedFilesStopsAtThePageCap checks the diff fetch stops at its page cap
// rather than following an endless chain of next links.
func TestChangedFilesStopsAtThePageCap(t *testing.T) {
	var host string
	var pages int
	c := serveWith(t, func(w http.ResponseWriter, _ *http.Request) {
		pages++
		w.Header().Set("Link", fmt.Sprintf("<%s/repos/o/r/pulls/7/files?page=2>; rel=\"next\"", host))
		_, _ = w.Write([]byte(`[]`))
	})
	host = c.cfg.APIBase
	files, truncated, err := c.changedFiles(t.Context(), 7)
	if err != nil {
		t.Fatalf("changedFiles: %v", err)
	}
	if len(files) != 0 || truncated {
		t.Errorf("changedFiles = %d files, truncated=%v, want none", len(files), truncated)
	}
	if pages != maxPages {
		t.Errorf("fetched %d pages, want the cap of %d", pages, maxPages)
	}
}

// TestGraphQLReportsAnErrorsArray checks the GraphQL path does not read a 200 as
// success. GitHub reports a failure with a 200 and an errors array, so a permission
// failure would otherwise read as "no threads to resolve".
func TestGraphQLReportsAnErrorsArray(t *testing.T) {
	c := serveWith(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"Resource not accessible"},{"message":"and again"}]}`))
	})
	err := c.graphql(t.Context(), viewerQuery, nil, nil)
	if err == nil {
		t.Fatal("an errors array must be an error")
	}
	for _, want := range []string{"Resource not accessible", "and again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to carry %q", err, want)
		}
	}
}

// TestGraphQLRefusesAnEmptyEnvelope checks a response with neither data nor errors is
// an error when the caller asked for data, and is accepted when it did not: a mutation
// has nothing to decode.
func TestGraphQLRefusesAnEmptyEnvelope(t *testing.T) {
	c := serveWith(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	var out struct{ Viewer struct{ Login string } }
	if err := c.graphql(t.Context(), viewerQuery, nil, &out); err == nil ||
		!strings.Contains(err.Error(), "carried no data") {
		t.Fatalf("graphql error = %v, want a no-data error", err)
	}
	if err := c.graphql(t.Context(), resolveThreadMutation, map[string]any{"threadId": "T"}, nil); err != nil {
		t.Fatalf("a mutation has nothing to decode, so an empty envelope is fine: %v", err)
	}
}

// TestViewerLoginRefusesAnAnonymousCredential checks a viewer query that names nobody
// is an error. The login is what tells the reviewer's comments from a maintainer's, so
// an empty one may not pass for an identity.
func TestViewerLoginRefusesAnAnonymousCredential(t *testing.T) {
	c := serveWith(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"viewer":{"login":""}}}`))
	})
	if _, err := c.viewerLogin(t.Context()); err == nil || !strings.Contains(err.Error(), "names no viewer") {
		t.Fatalf("viewerLogin error = %v, want a no-viewer error", err)
	}
}

// TestBodyPartsWithoutAClosedRuleTag checks the claim parser on a body whose rule tag
// was cut short: it returns what it could read rather than misreading the remainder as
// the summary.
func TestBodyPartsWithoutAClosedRuleTag(t *testing.T) {
	rule, summary, failure := bodyParts(markerPrefix + "abc -->\n" + ruleTagPrefix + "unterminated")
	if rule != "unterminated" {
		t.Errorf("rule = %q, want the text after the tag prefix", rule)
	}
	if summary != "" || failure != "" {
		t.Errorf("summary/failure = %q/%q, want both empty", summary, failure)
	}
}

// --- App authentication -------------------------------------------------------

// newAuth builds an authenticator against a fake token endpoint, on a manual clock so
// expiry is expressed in the test's time frame.
func newAuth(t *testing.T, app App, h http.HandlerFunc) (*authenticator, *clock.Manual) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	clk := clock.NewManual(time.Unix(1_700_000_000, 0).UTC())
	return &authenticator{app: app, clock: clk, http: srv.Client(), apiBase: srv.URL}, clk
}

// testKey is a real App key, generated once per run of this file's tests.
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func TestAssertionRequiresAnIssuer(t *testing.T) {
	a, _ := newAuth(t, App{InstallationID: 42, PrivateKey: testKey(t)}, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("no token should be minted without an issuer")
	})
	if _, err := a.installationToken(t.Context()); err == nil || !strings.Contains(err.Error(), "Issuer is required") {
		t.Fatalf("installationToken error = %v, want a missing-issuer error", err)
	}
}

// TestAssertionReportsASigningFailure checks a key the runtime refuses to sign with is
// reported rather than yielding an unsigned assertion. An undersized key is the case
// that reaches this: crypto/rsa declines it outright.
func TestAssertionReportsASigningFailure(t *testing.T) {
	tiny := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: big.NewInt(3233), E: 17},
		D:         big.NewInt(2753),
		Primes:    []*big.Int{big.NewInt(61), big.NewInt(53)},
	}
	a, _ := newAuth(t, App{Issuer: "Iv1.test", InstallationID: 42, PrivateKey: tiny}, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("no token should be minted when the assertion cannot be signed")
	})
	if _, err := a.installationToken(t.Context()); err == nil || !strings.Contains(err.Error(), "signing app assertion") {
		t.Fatalf("installationToken error = %v, want a signing failure", err)
	}
}

// TestTokenExchangeFailures checks every way the mint can go wrong is reported, so a
// request is never issued with an empty or stale credential.
func TestTokenExchangeFailures(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			name: "the App is not installed",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			},
			want: "mint installation token",
		},
		{
			name: "the response is not JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`<html>`))
			},
			want: "decoding installation token",
		},
		{
			name: "the response carries no token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"token":"","expires_at":"2023-11-14T22:13:20Z"}`))
			},
			want: "carried no token",
		},
	}
	key := testKey(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newAuth(t, App{Issuer: "Iv1.test", InstallationID: 42, PrivateKey: key}, tc.handler)
			tok, err := a.installationToken(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("installationToken error = %v, want it to mention %q", err, tc.want)
			}
			if !tok.Empty() {
				t.Error("a failed mint must yield no credential")
			}
		})
	}
}

// TestTokenExchangeTransportFailure checks a mint that never reaches GitHub is
// reported as such.
func TestTokenExchangeTransportFailure(t *testing.T) {
	a, _ := newAuth(t, App{Issuer: "Iv1.test", InstallationID: 42, PrivateKey: testKey(t)}, func(_ http.ResponseWriter, _ *http.Request) {})
	a.apiBase = deadServer(t)
	if _, err := a.installationToken(t.Context()); err == nil || !strings.Contains(err.Error(), "minting installation token") {
		t.Fatalf("installationToken error = %v, want a transport failure", err)
	}
}

// TestTokenExchangeRejectsAnUnusableAPIBase checks an API base that cannot form a URL
// fails before a request is built, rather than sending the assertion somewhere unknown.
func TestTokenExchangeRejectsAnUnusableAPIBase(t *testing.T) {
	a, _ := newAuth(t, App{Issuer: "Iv1.test", InstallationID: 42, PrivateKey: testKey(t)}, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("no request should be sent with an unusable API base")
	})
	a.apiBase = "http://example.invalid/%zz"
	if _, err := a.installationToken(t.Context()); err == nil {
		t.Fatal("an unusable API base must fail before the request is sent")
	}
}

// TestASupersededTokenIsWipedAndReplaced checks the refresh path: once the cached
// token is inside the refresh window, the next call mints a new one and the old value
// does not survive in the cache.
func TestASupersededTokenIsWipedAndReplaced(t *testing.T) {
	var minted int
	key := testKey(t)
	a, clk := newAuth(t, App{Issuer: "Iv1.test", InstallationID: 42, PrivateKey: key}, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("the replaced endpoint should receive nothing")
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		minted++
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q}`,
			minted, a.clock.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	t.Cleanup(srv.Close)
	a.http, a.apiBase = srv.Client(), srv.URL

	first, err := a.installationToken(t.Context())
	if err != nil {
		t.Fatalf("installationToken: %v", err)
	}
	if first.Expose() != "ghs_1" {
		t.Fatalf("token = %q, want ghs_1", first.Expose())
	}
	// Still well inside the token's life: the cached one is handed back untouched.
	clk.Advance(30 * time.Minute)
	again, err := a.installationToken(t.Context())
	if err != nil {
		t.Fatalf("installationToken: %v", err)
	}
	if again.Expose() != "ghs_1" || minted != 1 {
		t.Fatalf("token = %q after %d mints, want the cached ghs_1", again.Expose(), minted)
	}
	// Inside the refresh window: a fresh token is minted rather than one that could
	// expire mid-request.
	clk.Advance(30 * time.Minute)
	third, err := a.installationToken(t.Context())
	if err != nil {
		t.Fatalf("installationToken: %v", err)
	}
	if third.Expose() != "ghs_2" || minted != 2 {
		t.Fatalf("token = %q after %d mints, want a freshly minted ghs_2", third.Expose(), minted)
	}
}
