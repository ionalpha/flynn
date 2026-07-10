package github_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/tools/github"
)

// newTokenSet wires a Set that authenticates with a caller-supplied token rather
// than a GitHub App.
func newTokenSet(t *testing.T, hub *fakeHub, token string, mutate func(*github.Config)) *github.Set {
	t.Helper()
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)

	cfg := github.Config{
		Token:      secret.New(token),
		Owner:      "ionalpha",
		Repo:       "flynn",
		Number:     7,
		SelfLogin:  "reviewer[bot]",
		HTTPClient: srv.Client(),
		APIBase:    srv.URL,
		Clock:      clock.NewManual(time.Unix(1_700_000_000, 0).UTC()),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	hub.clk = cfg.Clock
	set, err := github.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return set
}

// --- the token path ----------------------------------------------------------

// A supplied token is carried on every request unchanged, and nothing is minted:
// there is no App, so there is no assertion to sign and no installation to exchange.
func TestTokenAuthCarriesTheTokenAndMintsNothing(t *testing.T) {
	hub := newFakeHub(t)
	hub.wantAuth = "Bearer ghp_supplied_token"
	set := newTokenSet(t, hub, "ghp_supplied_token", nil)

	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := hub.tokensMinted.Load(); got != 0 {
		t.Fatalf("minted %d installation tokens on the token path; want 0", got)
	}
	if hub.sawAuth.Load() == nil {
		t.Fatal("no request carried an Authorization header")
	}
}

// The whole toolset works on the token path, not merely the fetch: a review posted
// from a workflow needs to comment and to cast a verdict too.
func TestTokenAuthDrivesTheWholeToolset(t *testing.T) {
	hub := newFakeHub(t)
	set := newTokenSet(t, hub, "ghp_x", nil)

	in := `{"summary":"looks fine","findings":[{"path":"a.go","line":1,"rule":"r","summary":"s","failure":"f"}]}`
	if _, err := invoke(t, toolNamed(t, set, "github_comment"), in); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if _, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"REQUEST_CHANGES","conclusion":"Findings need addressing."}`); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got := hub.created.Load(); got != 1 {
		t.Fatalf("created = %d, want 1", got)
	}
	if got := hub.submittedBody.Load(); got != "REQUEST_CHANGES" {
		t.Fatalf("event = %v", got)
	}
	if got := hub.tokensMinted.Load(); got != 0 {
		t.Fatalf("minted %d tokens; want 0", got)
	}
}

// The approve gate is a property of the reviewer, not of how it authenticated. A
// token-authenticated reviewer is refused an approval it was not configured for,
// exactly as an App-authenticated one is.
func TestApproveGateHoldsOnTheTokenPath(t *testing.T) {
	hub := newFakeHub(t)
	set := newTokenSet(t, hub, "ghp_x", nil)

	_, err := invoke(t, toolNamed(t, set, "github_submit_review"), `{"event":"APPROVE","conclusion":"Nothing blocking."}`)
	if !errors.Is(err, github.ErrApproveNotEnabled) {
		t.Fatalf("want ErrApproveNotEnabled, got %v", err)
	}

	hub2 := newFakeHub(t)
	hub2.prAuthor = "reviewer[bot]"
	set2 := newTokenSet(t, hub2, "ghp_x", func(c *github.Config) { c.AllowApprove = true })
	_, err = invoke(t, toolNamed(t, set2, "github_submit_review"), `{"event":"APPROVE","conclusion":"Nothing blocking."}`)
	if !errors.Is(err, github.ErrSelfApproval) {
		t.Fatalf("want ErrSelfApproval, got %v", err)
	}
}

// --- choosing a credential ---------------------------------------------------

// Exactly one credential. Neither is a Config nobody finished writing; both is a
// Config that does not say which identity a review is published under, and guessing
// on the caller's behalf is worse than refusing.
func TestExactlyOneCredentialIsRequired(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	base := func() github.Config {
		return github.Config{Owner: "o", Repo: "r", Number: 7}
	}
	app := github.App{Issuer: "i", InstallationID: 1, PrivateKey: key}

	t.Run("neither", func(t *testing.T) {
		if _, err := github.New(base()); !errors.Is(err, github.ErrNoCredential) {
			t.Fatalf("want ErrNoCredential, got %v", err)
		}
	})
	t.Run("both", func(t *testing.T) {
		cfg := base()
		cfg.App, cfg.Token = app, secret.New("ghp_x")
		if _, err := github.New(cfg); !errors.Is(err, github.ErrAmbiguousCredential) {
			t.Fatalf("want ErrAmbiguousCredential, got %v", err)
		}
	})
	t.Run("token alone", func(t *testing.T) {
		cfg := base()
		cfg.Token = secret.New("ghp_x")
		if _, err := github.New(cfg); err != nil {
			t.Fatalf("a token alone must build: %v", err)
		}
	})
	t.Run("app alone", func(t *testing.T) {
		cfg := base()
		cfg.App = app
		if _, err := github.New(cfg); err != nil {
			t.Fatalf("an app alone must build: %v", err)
		}
	})
}

// A half-filled App is a mistake, not a request to fall back to something else.
// Failing at construction beats failing at the first API call.
func TestPartlyFilledAppIsRefusedRatherThanIgnored(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cases := map[string]github.App{
		"no issuer":       {InstallationID: 1, PrivateKey: key},
		"no installation": {Issuer: "i", PrivateKey: key},
		"no key":          {Issuer: "i", InstallationID: 1},
	}
	for name, app := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := github.New(github.Config{Owner: "o", Repo: "r", Number: 7, App: app})
			if err == nil {
				t.Fatal("want an error")
			}
			// It must not be mistaken for "no credential at all", which would send the
			// caller looking for a missing token rather than a missing App field.
			if errors.Is(err, github.ErrNoCredential) {
				t.Fatalf("a partly-filled App reported as no credential: %v", err)
			}
		})
	}
}

// An empty token is no token. It must not build a Set that sends "Bearer " and gets
// a confusing 401 on the first call.
func TestEmptyTokenIsNoCredential(t *testing.T) {
	cfg := github.Config{Owner: "o", Repo: "r", Number: 7, Token: secret.New("")}
	if _, err := github.New(cfg); !errors.Is(err, github.ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
}

// --- ParsePrivateKey ---------------------------------------------------------

func TestParsePrivateKeyAcceptsBothEncodings(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	pkcs1 := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	for name, in := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		t.Run(name, func(t *testing.T) {
			got, err := github.ParsePrivateKey(in)
			if err != nil {
				t.Fatalf("ParsePrivateKey: %v", err)
			}
			if !got.Equal(key) {
				t.Fatal("parsed a different key than was encoded")
			}
		})
	}
}

func TestParsePrivateKeyRejectsWhatIsNotAnAppKey(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ec key: %v", err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ec)
	if err != nil {
		t.Fatalf("marshal ec: %v", err)
	}

	cases := map[string][]byte{
		"empty":              {},
		"not pem":            []byte("ghp_this_is_a_token_not_a_key"),
		"pem header only":    pemArmour("RSA PRIVATE KEY", ""),
		"truncated body":     pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte{1, 2, 3}}),
		"an elliptic curve":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}),
		"a public key":       pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{4, 5, 6}}),
		"pem with no blocks": []byte("\n\n\n"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := github.ParsePrivateKey(in); err == nil {
				t.Fatal("want an error")
			}
		})
	}

	// The elliptic-curve case must say what it found, since a caller who pasted the
	// wrong key needs to know which one they pasted.
	_, err = github.ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}))
	if err == nil || !strings.Contains(err.Error(), "RSA") {
		t.Fatalf("the error should name the expected key type: %v", err)
	}
}

// A parsed key round-trips into a working App: the same key that signs an assertion
// is the one ParsePrivateKey returned, so the two halves are not merely
// independently correct.
func TestParsedKeyAuthenticatesAnApp(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	parsed, err := github.ParsePrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}

	hub := newFakeHub(t)
	set := newSet(t, hub, func(c *github.Config) { c.App.PrivateKey = parsed })
	if _, err := invoke(t, toolNamed(t, set, "github_pr_fetch"), `{}`); err != nil {
		t.Fatalf("fetch with a parsed key: %v", err)
	}
	if got := hub.tokensMinted.Load(); got != 1 {
		t.Fatalf("minted %d tokens, want 1", got)
	}
}

// pemArmour assembles PEM armour at run time. A secret scanner matches the armour
// rather than the key, so a literal header in a test fixture trips it; the
// alternative would be allowlisting a pattern, which would blind the scanner to a
// real key committed beside it.
func pemArmour(kind, body string) []byte {
	return []byte("-----BEGIN " + kind + "-----\n" + body + "-----END " + kind + "-----\n")
}
