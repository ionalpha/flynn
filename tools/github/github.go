// Package github reviews one pull request: fetch it and its diff, post
// comments, and submit a verdict. It runs no code and touches no working tree;
// requests go through a netguard client by default.
//
// The App installation holds pull_requests:write and contents:read and no
// contents:write, so the reviewer cannot push. APPROVE needs an opt-in in
// [Config] and is always refused on the reviewer's own pull request.
package github

import (
	"errors"
	"net/http"
	"sync"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/secret"
)

// DefaultAPIBase is GitHub's REST API root.
const DefaultAPIBase = "https://api.github.com"

// ErrApproveNotEnabled is returned for APPROVE without Config.AllowApprove.
// Approval is off by default so the reviewer does not become a merge gate on
// every repository it is installed on.
var ErrApproveNotEnabled = errors.New("github: APPROVE verdict is not enabled for this reviewer")

// ErrSelfApproval is returned for APPROVE on the reviewer's own pull request.
// No configuration enables it.
var ErrSelfApproval = errors.New("github: refusing to approve the reviewer's own pull request")

// ErrDiffTooLarge is returned when a pull request exceeds
// Config.MaxChangedLines without Config.ReviewOversize, because a huge diff is
// expensive and reviews badly.
var ErrDiffTooLarge = errors.New("github: pull request exceeds the review budget")

// ErrIncompleteDiff is returned for APPROVE on a truncated diff, because an
// approval claims the whole change was read. Blocking verdicts are allowed on
// a partial diff, since one defect is enough to say no.
var ErrIncompleteDiff = errors.New("github: refusing to approve a pull request whose diff was truncated")

// Config describes how a Set reaches GitHub and what it is permitted to do.
type Config struct {
	// App is the GitHub App installation to act as. Set exactly one of App and
	// Token. Only an App gives the reviewer an identity distinct from the
	// author, which a formal approval needs.
	App App

	// Token is a GITHUB_TOKEN or personal access token. A GITHUB_TOKEN review is
	// authored by github-actions[bot], which a repository refuses to let approve
	// unless "Allow GitHub Actions to create and approve pull requests" is on
	// (off by default).
	Token secret.Text

	// Owner and Repo fix the repository. No tool input names one, so the model
	// cannot redirect a review.
	Owner string
	Repo  string

	// Number fixes the pull request, which must be positive. No tool input names
	// one, so a diff mentioning "#123" cannot steer a comment or verdict there.
	Number int

	// SelfLogin is the reviewer's login, such as "my-reviewer[bot]"; APPROVE is
	// refused on its own pull requests. Empty disables the check, for tests only.
	SelfLogin string

	// AllowApprove permits APPROVE. Set it only from committed repository
	// configuration, never from anything a pull-request comment can reach.
	AllowApprove bool

	// MaxFiles caps the files a fetch returns and MaxPatchBytes each file's
	// patch, so a large pull request cannot flood the model's context. Zero
	// selects the defaults.
	MaxFiles      int
	MaxPatchBytes int

	// MaxChangedLines caps additions plus deletions, using GitHub's total for the
	// whole pull request so truncation cannot slip a diff under it. Zero selects
	// the default; negative disables it.
	MaxChangedLines int

	// ReviewOversize permits reviewing past MaxChangedLines, for an explicit human
	// request. It never permits approving: see ErrIncompleteDiff.
	ReviewOversize bool

	// HTTPClient issues API requests. Nil selects a netguard client with
	// netguard.PublicOnly, which refuses private, loopback and cloud-metadata
	// addresses after DNS resolution.
	HTTPClient *http.Client

	// APIBase overrides the REST API root, for tests or GitHub Enterprise. Empty
	// selects DefaultAPIBase.
	APIBase string

	// Clock times App token minting and expiry. Nil selects clock.System.
	Clock clock.Clock
}

const (
	defaultMaxFiles      = 300
	defaultMaxPatchBytes = 64 << 10

	// defaultMaxChangedLines is past the size where one review pass is useful.
	defaultMaxChangedLines = 3000
)

// Set is the GitHub review toolset for one pull request.
type Set struct {
	cfg    Config
	client *client

	// self is the authenticated login, resolved once. It identifies the
	// reviewer's own comments; a marker cannot, since anyone can copy one.
	self string

	// mu guards findings: the comment tool records them and the verdict reads
	// them, possibly on different goroutines. The key is always Config.Number,
	// and checks that a record and a verdict name the same pull request.
	mu       sync.Mutex
	findings map[int][]ReviewComment
}

// New fills cfg's defaults. It fails when the repository or pull request is
// missing, or when cfg names neither credential or both.
func New(cfg Config) (*Set, error) {
	if cfg.Owner == "" || cfg.Repo == "" {
		return nil, errors.New("github: Config.Owner and Config.Repo are required")
	}
	if cfg.Number <= 0 {
		return nil, errors.New("github: Config.Number (the pull request under review) must be positive")
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
	// Resolved last so an App authenticator gets the filled-in clock, client and
	// API base.
	auth, err := newTokenSource(cfg)
	if err != nil {
		return nil, err
	}
	return &Set{
		cfg:    cfg,
		client: &client{cfg: cfg, auth: auth},
	}, nil
}

// Tools returns the toolset as [mission.Tool] values. Their names are the
// capability names an archetype grants (github_pr_fetch, github_comment,
// github_submit_review), so a reviewer granted only github_pr_fetch can read
// and post nothing.
func (s *Set) Tools() []mission.Tool {
	return []mission.Tool{
		prFetchTool{s},
		commentTool{s},
		submitReviewTool{s},
	}
}
