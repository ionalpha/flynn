package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/llm"
)

// markerPrefix opens the HTML comment that keys a posted comment to the finding
// that produced it. It is invisible when rendered and stable across runs, so a
// second review over the same pull request reconciles its own comments in place
// instead of posting them again. Without such a key, every re-push duplicates
// every comment.
const markerPrefix = "<!-- flynn-review:"

// summaryMarker keys the single sticky summary comment a review maintains.
const summaryMarker = markerPrefix + "summary -->"

// Finding is one reviewable defect anchored to a line of the diff.
type Finding struct {
	// Path and Line locate the finding on the right-hand side of the diff.
	Path string `json:"path"`
	Line int    `json:"line"`

	// Rule names the check that produced the finding. It participates in the
	// comment's identity, so re-running a review updates a finding in place rather
	// than posting it twice.
	Rule string `json:"rule"`

	// Summary states the defect in one sentence.
	Summary string `json:"summary"`

	// Failure is the concrete scenario the defect causes: inputs or state, and the
	// wrong output or crash that results. A finding without one is not postable,
	// because a comment that cannot name what breaks is a nitpick.
	Failure string `json:"failure"`
}

// key is the finding's stable identity: its location and the rule that found it.
// The body is excluded on purpose, so rewording a finding updates the existing
// comment instead of orphaning it.
func (f Finding) key() string {
	sum := sha256.Sum256([]byte(f.Path + "\x00" + strconv.Itoa(f.Line) + "\x00" + f.Rule))
	return hex.EncodeToString(sum[:6])
}

// marker is the invisible identity tag embedded in the finding's comment body.
func (f Finding) marker() string { return markerPrefix + f.key() + " -->" }

// render builds the comment body for a finding, marker included.
func (f Finding) render() string {
	var b strings.Builder
	b.WriteString(f.marker())
	b.WriteString("\n**")
	b.WriteString(f.Rule)
	b.WriteString("** ")
	b.WriteString(f.Summary)
	b.WriteString("\n\n")
	b.WriteString(f.Failure)
	return b.String()
}

// validate reports why a finding cannot be posted, or nil when it can.
func (f Finding) validate() error {
	switch {
	case f.Path == "":
		return errors.New("github: finding has no path")
	case f.Line <= 0:
		return fmt.Errorf("github: finding on %s has no line", f.Path)
	case f.Rule == "":
		return fmt.Errorf("github: finding at %s:%d has no rule", f.Path, f.Line)
	case strings.TrimSpace(f.Summary) == "":
		return fmt.Errorf("github: finding at %s:%d has no summary", f.Path, f.Line)
	case strings.TrimSpace(f.Failure) == "":
		return fmt.Errorf("github: finding at %s:%d has no failure scenario", f.Path, f.Line)
	}
	return nil
}

// markerIn extracts the reviewer's marker from a comment body, or "" when it carries
// none, which is how a human's comment is distinguished from the reviewer's.
func markerIn(body string) string {
	i := strings.Index(body, markerPrefix)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "-->")
	if j < 0 {
		return ""
	}
	return body[i : i+j+len("-->")]
}

// --- pr_fetch ---------------------------------------------------------------

type prFetchTool struct{ s *Set }

func (prFetchTool) Def() llm.Tool {
	return llm.Tool{
		Name: "github_pr_fetch",
		Description: "Fetch a pull request: its metadata, the list of changed files with their diff patches, " +
			"and the identities of findings this reviewer has already posted. Re-proposing an already-posted " +
			"finding is a duplicate; the existing comment is updated instead.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["number"],
  "properties": {
    "number": {"type": "integer", "description": "Pull request number."}
  },
  "additionalProperties": false
}`),
	}
}

// prFetchResult is what a fetch hands the model.
type prFetchResult struct {
	Number         int           `json:"number"`
	Title          string        `json:"title"`
	Body           string        `json:"body"`
	State          string        `json:"state"`
	Draft          bool          `json:"draft"`
	AuthorLogin    string        `json:"author_login"`
	HeadSHA        string        `json:"head_sha"`
	ChangedFiles   int           `json:"changed_files"`
	ChangedLines   int           `json:"changed_lines"`
	Files          []ChangedFile `json:"files"`
	FilesTruncated bool          `json:"files_truncated,omitempty"`

	// DiffComplete reports whether every changed file and every patch reached the
	// review intact. When it is false the review is working from a partial picture,
	// and an APPROVE verdict will be refused.
	DiffComplete bool `json:"diff_complete"`

	PostedFindings []string `json:"posted_findings,omitempty"`
}

// oversize reports whether the pull request exceeds the configured review budget.
// A negative budget disables the check.
func (s *Set) oversize(pr PullRequest) bool {
	return s.cfg.MaxChangedLines > 0 && pr.ChangedLines() > s.cfg.MaxChangedLines
}

// diffCoverage reports whether the whole diff was available to the review, and why
// not when it was not. It consults GitHub's own file count, so a diff cannot appear
// complete merely because the fetch stopped early.
func (s *Set) diffCoverage(ctx context.Context, pr PullRequest) (bool, string, error) {
	if pr.ChangedFiles > s.cfg.MaxFiles {
		return false, fmt.Sprintf("%d changed files exceeds the %d-file fetch cap",
			pr.ChangedFiles, s.cfg.MaxFiles), nil
	}
	files, truncated, err := s.client.changedFiles(ctx, pr.Number)
	if err != nil {
		return false, "", err
	}
	if truncated {
		return false, "the changed-file list was truncated", nil
	}
	for _, f := range files {
		if f.PatchTruncated {
			return false, fmt.Sprintf("the patch for %s was truncated", f.Filename), nil
		}
	}
	return true, "", nil
}

func (t prFetchTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if in.Number <= 0 {
		return "", errors.New("github: pull request number must be positive")
	}
	pr, err := t.s.client.pullRequest(ctx, in.Number)
	if err != nil {
		return "", err
	}
	// Refuse a runaway diff before spending a single model token on it. The caller
	// opts in explicitly through Config.ReviewOversize when a big change really does
	// need reviewing.
	if t.s.oversize(pr) && !t.s.cfg.ReviewOversize {
		return "", fmt.Errorf("%w: %d changed lines exceeds the %d-line budget",
			ErrDiffTooLarge, pr.ChangedLines(), t.s.cfg.MaxChangedLines)
	}
	files, truncated, err := t.s.client.changedFiles(ctx, in.Number)
	if err != nil {
		return "", err
	}
	existing, err := t.s.client.reviewComments(ctx, in.Number)
	if err != nil {
		return "", err
	}
	posted := make([]string, 0, len(existing))
	for _, c := range existing {
		if m := markerIn(c.Body); m != "" {
			posted = append(posted, m)
		}
	}
	sort.Strings(posted)

	complete := !truncated && pr.ChangedFiles <= t.s.cfg.MaxFiles
	for _, f := range files {
		if f.PatchTruncated {
			complete = false
			break
		}
	}

	out := prFetchResult{
		Number: pr.Number, Title: pr.Title, Body: pr.Body, State: pr.State, Draft: pr.Draft,
		AuthorLogin: pr.AuthorLogin, HeadSHA: pr.HeadSHA,
		ChangedFiles: pr.ChangedFiles, ChangedLines: pr.ChangedLines(),
		Files: files, FilesTruncated: truncated, DiffComplete: complete,
		PostedFindings: posted,
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// --- comment ----------------------------------------------------------------

type commentTool struct{ s *Set }

func (commentTool) Def() llm.Tool {
	return llm.Tool{
		Name: "github_comment",
		Description: "Post review findings as inline comments and maintain the single summary comment. " +
			"Re-running reconciles: a finding already posted at the same path, line, and rule is updated in " +
			"place, never duplicated. Every finding must carry a concrete failure scenario or it is refused.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["number"],
  "properties": {
    "number": {"type": "integer", "description": "Pull request number."},
    "summary": {"type": "string", "description": "The review summary. Replaces the previous summary comment in place."},
    "findings": {
      "type": "array",
      "description": "Inline findings. Omit for a summary-only review.",
      "items": {
        "type": "object",
        "required": ["path", "line", "rule", "summary", "failure"],
        "properties": {
          "path": {"type": "string", "description": "File path as it appears in the diff."},
          "line": {"type": "integer", "description": "Line number on the right-hand side of the diff."},
          "rule": {"type": "string", "description": "Short identifier of the check that found this."},
          "summary": {"type": "string", "description": "One sentence stating the defect."},
          "failure": {"type": "string", "description": "Concrete inputs or state, and the wrong output or crash that results."}
        },
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}`),
	}
}

func (t commentTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Number   int       `json:"number"`
		Summary  string    `json:"summary"`
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if in.Number <= 0 {
		return "", errors.New("github: pull request number must be positive")
	}
	// Validate every finding before posting any, so a malformed batch does not leave
	// the pull request half-reviewed.
	for _, f := range in.Findings {
		if err := f.validate(); err != nil {
			return "", err
		}
	}

	created, updated, err := t.reconcileFindings(ctx, in.Number, in.Findings)
	if err != nil {
		return "", err
	}
	if s := strings.TrimSpace(in.Summary); s != "" {
		if err := t.reconcileSummary(ctx, in.Number, s); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("posted %d new finding(s), updated %d existing", created, updated), nil
}

// reconcileFindings creates comments for new findings and updates the ones already
// posted, keyed by marker.
func (t commentTool) reconcileFindings(ctx context.Context, number int, findings []Finding) (int, int, error) {
	if len(findings) == 0 {
		return 0, 0, nil
	}
	pr, err := t.s.client.pullRequest(ctx, number)
	if err != nil {
		return 0, 0, err
	}
	existing, err := t.s.client.reviewComments(ctx, number)
	if err != nil {
		return 0, 0, err
	}
	byMarker := make(map[string]int64, len(existing))
	for _, c := range existing {
		if m := markerIn(c.Body); m != "" {
			byMarker[m] = c.ID
		}
	}

	var created, updated int
	for _, f := range findings {
		if id, ok := byMarker[f.marker()]; ok {
			if err := t.s.client.updateReviewComment(ctx, id, f); err != nil {
				return created, updated, err
			}
			updated++
			continue
		}
		if err := t.s.client.createReviewComment(ctx, number, pr.HeadSHA, f); err != nil {
			return created, updated, err
		}
		created++
	}
	return created, updated, nil
}

// reconcileSummary rewrites the sticky summary comment, or posts it the first time.
func (t commentTool) reconcileSummary(ctx context.Context, number int, summary string) error {
	body := summaryMarker + "\n" + summary
	existing, err := t.s.client.issueComments(ctx, number)
	if err != nil {
		return err
	}
	for _, c := range existing {
		if markerIn(c.Body) == summaryMarker {
			return t.s.client.updateIssueComment(ctx, c.ID, body)
		}
	}
	return t.s.client.createIssueComment(ctx, number, body)
}

// --- submit_review ----------------------------------------------------------

type submitReviewTool struct{ s *Set }

func (submitReviewTool) Def() llm.Tool {
	return llm.Tool{
		Name: "github_submit_review",
		Description: "Submit the formal review verdict on a pull request: COMMENT, REQUEST_CHANGES, or APPROVE. " +
			"APPROVE is refused unless the reviewer was configured to allow it, and is always refused on a pull " +
			"request the reviewer authored.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["number", "event"],
  "properties": {
    "number": {"type": "integer", "description": "Pull request number."},
    "event": {"type": "string", "enum": ["COMMENT", "REQUEST_CHANGES", "APPROVE"], "description": "The verdict."},
    "body": {"type": "string", "description": "Optional verdict message."}
  },
  "additionalProperties": false
}`),
	}
}

func (t submitReviewTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Number int    `json:"number"`
		Event  string `json:"event"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if in.Number <= 0 {
		return "", errors.New("github: pull request number must be positive")
	}
	switch in.Event {
	case "COMMENT", "REQUEST_CHANGES":
	case "APPROVE":
		if !t.s.cfg.AllowApprove {
			return "", ErrApproveNotEnabled
		}
	default:
		return "", fmt.Errorf("github: unknown review event %q", in.Event)
	}

	pr, err := t.s.client.pullRequest(ctx, in.Number)
	if err != nil {
		return "", err
	}
	// The self-approval check reads the live author rather than trusting the caller,
	// so a model cannot talk its way past it by misreporting who opened the change.
	if in.Event == "APPROVE" && t.s.cfg.SelfLogin != "" && pr.AuthorLogin == t.s.cfg.SelfLogin {
		return "", ErrSelfApproval
	}
	// An APPROVE asserts the change was reviewed. If the diff was truncated on the
	// way in, that assertion is false, and it is the assertion a signed verdict would
	// go on to attest. Blocking verdicts stay available on partial evidence: seeing
	// one real defect is enough to say no, but seeing most of a diff is never enough
	// to say yes. No configuration relaxes this.
	if in.Event == "APPROVE" {
		complete, why, err := t.s.diffCoverage(ctx, pr)
		if err != nil {
			return "", err
		}
		if !complete {
			return "", fmt.Errorf("%w: %s", ErrIncompleteDiff, why)
		}
	}
	if err := t.s.client.submitReview(ctx, in.Number, pr.HeadSHA, in.Event, in.Body); err != nil {
		return "", err
	}
	return fmt.Sprintf("submitted %s on #%d", in.Event, in.Number), nil
}
