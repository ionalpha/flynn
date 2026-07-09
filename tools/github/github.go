// Package github is the agent's pull-request review surface: fetch a pull
// request and its diff, post inline and summary comments, and submit a formal
// review verdict. It is the read-and-review half of the GitHub tool, distinct
// from cloning a contributor's branch and running its code, which stays behind
// the sandbox.
//
// The tools here hold no host access. They call the GitHub REST API through an
// injected HTTP client, which defaults to a netguard-backed client so the egress
// policy applies to every request. Nothing here executes code, reads the working
// tree, or spawns a process: a review is a read of a diff and a write to an API.
//
// Authority is deliberately narrow. A Set authenticates as a GitHub App
// installation, which holds pull_requests:write and contents:read and no
// contents:write, so the reviewer can never push to a branch. Submitting an
// APPROVE verdict is refused unless the caller opts in through Config, and is
// always refused on a pull request the reviewer itself authored.
package github

import (
	"errors"
	"net/http"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/secret"
)

// DefaultAPIBase is GitHub's REST API root, used when Config leaves APIBase empty.
const DefaultAPIBase = "https://api.github.com"

// ErrApproveNotEnabled is returned when a submit_review call asks for an APPROVE
// verdict but the Set was built without Config.AllowApprove. Approval is off by
// default: a reviewer that approves unless told otherwise silently becomes a
// merge gate on every repository it is installed on.
var ErrApproveNotEnabled = errors.New("github: APPROVE verdict is not enabled for this reviewer")

// ErrSelfApproval is returned when the reviewer would approve a pull request it
// authored itself. A self-approval is not a second opinion, and no configuration
// enables it.
var ErrSelfApproval = errors.New("github: refusing to approve the reviewer's own pull request")

// ErrDiffTooLarge is returned when a pull request exceeds the configured review
// budget and the caller has not opted in to reviewing it anyway. A runaway diff
// costs real money to review and produces a worse review at the end of it, so the
// default is to refuse and let a human ask for it explicitly.
var ErrDiffTooLarge = errors.New("github: pull request exceeds the review budget")

// ErrIncompleteDiff is returned when an APPROVE verdict is attempted on a pull
// request whose diff did not reach the review in full. An approval asserts the
// change was reviewed, so approving a diff that was truncated on the way in states
// something untrue. Blocking verdicts stay available on partial evidence: one real
// defect is enough to say no, but most of a diff is never enough to say yes.
var ErrIncompleteDiff = errors.New("github: refusing to approve a pull request whose diff was truncated")

// Config describes how a Set reaches GitHub and what it is permitted to do.
type Config struct {
	// App identifies the GitHub App and the installation the Set acts as. Set exactly
	// one of App and Token.
	//
	// The App path gives the reviewer an identity of its own, distinct from whoever
	// authored the pull request, which is what a formal approval requires.
	App App

	// Token authenticates as whoever issued it: a workflow's ambient GITHUB_TOKEN, or
	// a personal access token. Set exactly one of App and Token.
	//
	// A review posted with a workflow's GITHUB_TOKEN is authored by
	// github-actions[bot], and a repository refuses an approving review from that
	// identity unless an owner has enabled "Allow GitHub Actions to create and approve
	// pull requests", which is off by default. So this path can comment and request
	// changes anywhere, and can approve only where that setting was deliberately
	// turned on.
	Token secret.Text

	// Owner and Repo bound the Set to a single repository. Every tool call operates
	// on this repository; the model cannot redirect a review at another one because
	// the coordinates are not part of any tool's input schema.
	Owner string
	Repo  string

	// SelfLogin is the reviewer's own login, such as "my-reviewer[bot]". When a pull
	// request's author matches it, an APPROVE verdict is refused. Empty disables the
	// check, which is only correct in tests.
	SelfLogin string

	// AllowApprove permits the APPROVE verdict. It is off by default on purpose, and
	// a host should set it only from committed repository configuration, never from
	// a value a pull-request comment can reach.
	AllowApprove bool

	// MaxFiles caps how many changed files a fetch returns, and MaxPatchBytes caps
	// each file's patch, so a large pull request cannot flood the model's context.
	// Zero selects the defaults.
	MaxFiles      int
	MaxPatchBytes int

	// MaxChangedLines is the review budget: a pull request whose additions plus
	// deletions exceed it is refused rather than reviewed, because a runaway diff
	// costs real money and yields a worse review. Zero selects the default. Negative
	// disables the budget, which is what an explicit "review it anyway" invocation
	// passes.
	//
	// The count is GitHub's own total for the whole pull request, so a diff cannot
	// slip under the budget by being truncated on the way in.
	MaxChangedLines int

	// ReviewOversize permits reviewing a pull request that exceeds MaxChangedLines.
	// It exists for the explicit, human-initiated "yes, review the big one" path. It
	// does not permit approving one: see ErrIncompleteDiff.
	ReviewOversize bool

	// HTTPClient issues the API requests. Nil selects a netguard-backed client with a
	// public-only egress policy, which refuses connections to private, loopback, and
	// cloud-metadata addresses after DNS resolution.
	HTTPClient *http.Client

	// APIBase overrides the REST API root. Empty selects DefaultAPIBase. Tests point
	// it at an httptest server; a GitHub Enterprise host points it at its own API.
	APIBase string

	// Clock supplies the current time for App token minting and expiry. Nil selects
	// clock.System.
	Clock clock.Clock
}

// Defaults for the context-protecting caps in Config.
const (
	defaultMaxFiles      = 300
	defaultMaxPatchBytes = 64 << 10

	// defaultMaxChangedLines is the review budget. A pull request larger than this is
	// past the point where a single review pass is useful, and is where an unattended
	// reviewer starts spending real money on a diff nobody asked it to read.
	defaultMaxChangedLines = 3000
)

// Set is the GitHub review toolset for one repository. Construct it with New and
// hand its tools to a mission executor.
type Set struct {
	cfg    Config
	client *client
}

// New builds a review toolset for the repository named in cfg, filling defaults
// for the HTTP client, API base, clock, and context caps. It returns an error when
// cfg omits the repository coordinates, or names neither credential, or names both.
func New(cfg Config) (*Set, error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return nil, errors.New("github: Config.Owner and Config.Repo are required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = netguard.Client(netguard.PublicOnly())
	}
	if cfg.APIBase == "" {
		cfg.APIBase = DefaultAPIBase
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = defaultMaxFiles
	}
	if cfg.MaxPatchBytes <= 0 {
		cfg.MaxPatchBytes = defaultMaxPatchBytes
	}
	if cfg.MaxChangedLines == 0 {
		cfg.MaxChangedLines = defaultMaxChangedLines
	}
	// The credential is resolved last, so the defaults above are already filled in and
	// an App authenticator is built with the same clock, client, and API base every
	// other request uses.
	auth, err := newTokenSource(cfg)
	if err != nil {
		return nil, err
	}
	return &Set{
		cfg:    cfg,
		client: &client{cfg: cfg, auth: auth},
	}, nil
}

// Tools returns the review toolset as mission.Tools, ready to register with an
// executor. The names match the capability names an Agent archetype grants:
// github_pr_fetch, github_comment, and github_submit_review. A tool the archetype
// does not list is refused at the dispatch waist, so a reviewer granted only
// github_pr_fetch can read a diff and post nothing.
func (s *Set) Tools() []mission.Tool {
	return []mission.Tool{
		prFetchTool{s},
		commentTool{s},
		submitReviewTool{s},
	}
}
