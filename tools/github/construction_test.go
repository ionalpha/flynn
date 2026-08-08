package github_test

// What New accepts and what the resulting toolset exposes. A reviewer's authority is
// the tools it holds, so the surface is asserted exactly: an accidental addition here
// widens what an installed reviewer may do.

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"

	"github.com/ionalpha/flynn/tools/github"
)

func TestNewRejectsIncompleteConfig(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	full := func() github.Config {
		return github.Config{
			App:   github.App{Issuer: "i", InstallationID: 1, PrivateKey: key},
			Owner: "o", Repo: "r", Number: 7,
		}
	}
	cases := map[string]func(*github.Config){
		"no owner":        func(c *github.Config) { c.Owner = "" },
		"no repo":         func(c *github.Config) { c.Repo = "" },
		"no number":       func(c *github.Config) { c.Number = 0 },
		"no installation": func(c *github.Config) { c.App.InstallationID = 0 },
		"no private key":  func(c *github.Config) { c.App.PrivateKey = nil },
	}
	for name, invalidate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := full()
			invalidate(&cfg)
			if _, err := github.New(cfg); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	if _, err := github.New(full()); err != nil {
		t.Fatalf("a complete config must build: %v", err)
	}
}

// The toolset exposes exactly the three review capabilities and nothing else. A
// reviewer's authority is the tools it holds, so an accidental addition here is a
// widening of authority.
func TestToolsetSurfaceIsExactlyTheReviewCapabilities(t *testing.T) {
	hub := newFakeHub(t)
	set := newSet(t, hub, nil)

	want := map[string]bool{"github_pr_fetch": true, "github_comment": true, "github_submit_review": true}
	got := map[string]bool{}
	for _, tl := range set.Tools() {
		got[tl.Def().Name] = true
	}
	if len(got) != len(want) {
		t.Fatalf("toolset = %v, want exactly %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing tool %q", name)
		}
	}
}
