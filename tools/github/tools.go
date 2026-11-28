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

	"github.com/ionalpha/flynn/internal/verdict"
	"github.com/ionalpha/flynn/llm"
)

// markerPrefix opens the invisible HTML comment that keys a posted comment to
// its finding. The key is stable across runs, so a re-review updates its
// comments in place instead of duplicating them on every push.
const markerPrefix = "<!-- flynn-review:"

// Finding is one reviewable defect anchored to a line of the diff.
type Finding struct {
	// Path and Line locate the finding on the right-hand side of the diff.
	Path string `json:"path"`
	Line int    `json:"line"`

	// Rule names the check that found it, and is part of the comment's identity.
	Rule string `json:"rule"`

	// Summary states the defect in one sentence.
	Summary string `json:"summary"`

	// Failure is the inputs or state and the wrong output or crash that results.
	// It is required, because a comment that cannot name what breaks is a
	// nitpick.
	Failure string `json:"failure"`
}

// fingerprint hashes path, line and rule. The body is excluded so rewording a
// finding updates its comment instead of orphaning it. The inputs are public
// and printed in the comment, so a fast hash is correct here.
func (f Finding) fingerprint() string {
	sum := sha256.Sum256([]byte(f.Path + "\x00" + strconv.Itoa(f.Line) + "\x00" + f.Rule))
	return hex.EncodeToString(sum[:6])
}

func (f Finding) marker() string { return markerPrefix + f.fingerprint() + " -->" }

// ruleTagPrefix opens the invisible HTML comment holding a finding's rule, so
// the author sees only the defect while a re-review can still read the rule.
const ruleTagPrefix = "<!-- flynn-rule:"

// render writes the format bodyParts reads back.
func (f Finding) render() string {
	var b strings.Builder
	b.WriteString(f.marker())
	b.WriteByte('\n')
	b.WriteString(ruleTagPrefix)
	b.WriteString(f.Rule)
	b.WriteString(" -->\n")
	b.WriteString(f.Summary)
	b.WriteString("\n\n")
	b.WriteString(f.Failure)
	return b.String()
}

func (f Finding) validate() error {
	switch {
	case f.Path == "":
		return errors.New("github: finding has no path")
	case f.Line <= 0:
		return fmt.Errorf("github: finding on %s has no line", f.Path)
	case f.Rule == "":
		return fmt.Errorf("github: finding at %s:%d has no rule", f.Path, f.Line)
	case strings.Contains(f.Rule, "-->") || strings.ContainsRune(f.Rule, '\n'):
		// A "-->" or newline would close the rule's HTML comment early.
		return fmt.Errorf("github: finding at %s:%d has a rule that cannot be embedded: %q", f.Path, f.Line, f.Rule)
	case strings.TrimSpace(f.Summary) == "":
		return fmt.Errorf("github: finding at %s:%d has no summary", f.Path, f.Line)
	case strings.TrimSpace(f.Failure) == "":
		return fmt.Errorf("github: finding at %s:%d has no failure scenario", f.Path, f.Line)
	}
	return nil
}

// PostedFinding is a finding already on the pull request. Path, Line and Rule
// regenerate the marker, so a repost updates the comment. Summary and Failure
// are the earlier claim, so the reviewer can check it against the current diff
// instead of reposting on faith.
type PostedFinding struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Rule    string `json:"rule"`
	Summary string `json:"summary,omitempty"`
	Failure string `json:"failure,omitempty"`
}

// bodyParts reads back what render wrote:
//
//	<marker>
//	<rule tag>
//	<summary>
//
//	<failure>
//
// A body without the rule tag, such as a human's comment, returns all "".
func bodyParts(body string) (rule, summary, failure string) {
	i := strings.Index(body, ruleTagPrefix)
	if i < 0 {
		return "", "", ""
	}
	rule, rest, ok := strings.Cut(body[i+len(ruleTagPrefix):], " -->\n")
	if !ok {
		return rule, "", ""
	}
	summary, failure, _ = strings.Cut(rest, "\n\n")
	return rule, summary, failure
}

// markerIn returns the marker in body, or "" if there is none.
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
		Description: "Fetch the pull request under review: its metadata, the list of changed files with their " +
			"diff patches, and the findings this reviewer has already posted, each with the rule, summary, and " +
			"failure scenario it claimed. The pull request is fixed for this run; the fetch takes no arguments " +
			"and echoes its number back. For each posted finding, decide against the fetched diff whether the " +
			"defect is still there. Repost the ones that are (the existing comment is updated in place, never " +
			"duplicated) and drop the ones the change has fixed. Do not repost a finding without checking; a " +
			"stale finding reposted is a false objection kept alive.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {},
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

	// DiffComplete is false when any file or patch was truncated; APPROVE is then
	// refused.
	DiffComplete bool `json:"diff_complete"`

	// PostedFindings are this reviewer's existing findings with their claims.
	PostedFindings []PostedFinding `json:"posted_findings,omitempty"`
}

func (s *Set) oversize(pr PullRequest) bool {
	return s.cfg.MaxChangedLines > 0 && pr.ChangedLines() > s.cfg.MaxChangedLines
}

// diffCoverage checks GitHub's own file count, so a fetch that stopped early
// does not look complete. A diff that could not be fetched is [verdict.Unknown]:
// it is no evidence that the diff is incomplete, and none that it is whole.
func (s *Set) diffCoverage(ctx context.Context, pr PullRequest) verdict.Verdict {
	if pr.ChangedFiles > s.cfg.MaxFiles {
		return verdict.Fail(fmt.Sprintf("%d changed files exceeds the %d-file fetch cap",
			pr.ChangedFiles, s.cfg.MaxFiles))
	}
	files, truncated, err := s.client.changedFiles(ctx, pr.Number)
	if err != nil {
		return verdict.Unevaluated(err)
	}
	if truncated {
		return verdict.Fail("the changed-file list was truncated")
	}
	for _, f := range files {
		if f.PatchTruncated {
			return verdict.Fail(fmt.Sprintf("the patch for %s was truncated", f.Filename))
		}
	}
	return verdict.Pass()
}

func (t prFetchTool) Invoke(ctx context.Context, _ json.RawMessage) (string, error) {
	number := t.s.cfg.Number
	pr, err := t.s.client.pullRequest(ctx, number)
	if err != nil {
		return "", err
	}
	// Refused before any model token is spent on it.
	if t.s.oversize(pr) && !t.s.cfg.ReviewOversize {
		return "", fmt.Errorf("%w: %d changed lines exceeds the %d-line budget",
			ErrDiffTooLarge, pr.ChangedLines(), t.s.cfg.MaxChangedLines)
	}
	files, truncated, err := t.s.client.changedFiles(ctx, number)
	if err != nil {
		return "", err
	}
	existing, err := t.s.client.reviewComments(ctx, number)
	if err != nil {
		return "", err
	}
	// Anyone can copy a marker, so the author is checked too. Without a known
	// login no findings are reported, so the reviewer never adopts a
	// maintainer's comment.
	self, err := t.s.selfLogin(ctx)
	if err != nil {
		return "", err
	}
	posted := postedFindings(existing, self)

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

// postedFindings is sorted so an unchanged pull request reads the same twice.
func postedFindings(existing []ReviewComment, self string) []PostedFinding {
	posted := make([]PostedFinding, 0, len(existing))
	for _, c := range existing {
		if markerIn(c.Body) == "" || !sameLogin(c.User.Login, self) {
			continue
		}
		// GitHub sends a null line (decoded as 0) when the anchor left the diff.
		// validate would refuse a repost, and GitHub already marks the thread
		// outdated, so it is skipped.
		if c.Line <= 0 {
			continue
		}
		rule, summary, failure := bodyParts(c.Body)
		posted = append(posted, PostedFinding{
			Path: c.Path, Line: c.Line, Rule: rule, Summary: summary, Failure: failure,
		})
	}
	// Rule breaks path and line ties, because sort.Slice is not stable.
	sort.Slice(posted, func(i, j int) bool {
		if posted[i].Path != posted[j].Path {
			return posted[i].Path < posted[j].Path
		}
		if posted[i].Line != posted[j].Line {
			return posted[i].Line < posted[j].Line
		}
		return posted[i].Rule < posted[j].Rule
	})
	return posted
}

// --- comment ----------------------------------------------------------------

type commentTool struct{ s *Set }

func (commentTool) Def() llm.Tool {
	return llm.Tool{
		Name: "github_comment",
		Description: "Post review findings as inline comments on the lines they concern. " +
			"Re-running reconciles: a finding already posted at the same path, line, and rule is updated in " +
			"place, never duplicated, so post again every finding whose defect the current diff still shows. " +
			"A finding you omit is retracted: its conversation is resolved, or folded away as outdated, which " +
			"is what you want for a defect the change has fixed and not for one still present. Decide per " +
			"finding against the diff; do not repost one you have not rechecked. " +
			"Every finding must carry a concrete failure scenario or it is refused. " +
			"There is no separate summary comment: a finding lives on its line, and the verdict carries the " +
			"one-line conclusion.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["findings"],
  "properties": {
    "findings": {
      "type": "array",
      "description": "Inline findings, each anchored to the line it concerns.",
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

// Invoke posts findings inline only. The conclusion goes in the verdict, so
// the main thread gets no summary that repeats both.
func (t commentTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	if len(in.Findings) == 0 {
		return "", errors.New("github: no findings to post; a review with nothing to say submits its verdict directly")
	}
	// Validate all before posting any, so a bad batch leaves nothing half done.
	for _, f := range in.Findings {
		if err := f.validate(); err != nil {
			return "", err
		}
	}

	created, updated, err := t.reconcileFindings(ctx, t.s.cfg.Number, in.Findings)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("posted %d new finding(s), updated %d existing", created, updated), nil
}

// reconcileFindings updates existing comments by marker and creates the rest.
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
	// Only the reviewer's own comments are matched, because a maintainer quoting
	// a finding copies its marker. Without a known login, nothing is adopted and
	// new comments are posted.
	self, err := t.s.selfLogin(ctx)
	if err != nil {
		return 0, 0, err
	}
	byMarker := make(map[string]int64, len(existing))
	for _, c := range existing {
		if !sameLogin(c.User.Login, self) {
			continue
		}
		if m := markerIn(c.Body); m != "" {
			byMarker[m] = c.ID
		}
	}

	var created, updated int
	for _, f := range findings {
		if id, ok := byMarker[f.marker()]; ok {
			c, err := t.s.client.updateReviewComment(ctx, id, f)
			if err != nil {
				return created, updated, err
			}
			t.s.recordFinding(number, c)
			updated++
			continue
		}
		c, err := t.s.client.createReviewComment(ctx, number, pr.HeadSHA, f)
		if err != nil {
			return created, updated, err
		}
		t.s.recordFinding(number, c)
		created++
	}
	return created, updated, nil
}

// --- submit_review ----------------------------------------------------------

type submitReviewTool struct{ s *Set }

func (submitReviewTool) Def() llm.Tool {
	return llm.Tool{
		Name: "github_submit_review",
		Description: "Submit the formal review verdict on a pull request: COMMENT, REQUEST_CHANGES, or APPROVE. " +
			"The verdict links to the findings already posted inline; do not restate them in the body. " +
			"APPROVE is refused unless the reviewer was configured to allow it, and is always refused on a pull " +
			"request the reviewer authored.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "required": ["event", "conclusion"],
  "properties": {
    "event": {"type": "string", "enum": ["COMMENT", "REQUEST_CHANGES", "APPROVE"], "description": "The verdict."},
    "conclusion": {"type": "string", "description": "One sentence stating what you concluded. Not a summary of the findings: they are linked automatically."}
  },
  "additionalProperties": false
}`),
	}
}

// verdictBody is the conclusion plus a link to each finding, sorted by file and
// line. The tool builds the list because GitHub requires a body on COMMENT and
// REQUEST_CHANGES, and a model given a free field restates its findings.
func verdictBody(conclusion string, findings []ReviewComment) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(conclusion))
	if len(findings) == 0 {
		return b.String()
	}
	ordered := append([]ReviewComment(nil), findings...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Path != ordered[j].Path {
			return ordered[i].Path < ordered[j].Path
		}
		return ordered[i].Line < ordered[j].Line
	})
	if len(ordered) == 1 {
		b.WriteString("\n\nOne finding:\n")
	} else {
		fmt.Fprintf(&b, "\n\n%d findings:\n", len(ordered))
	}
	for _, c := range ordered {
		anchor := fmt.Sprintf("`%s:%d`", c.Path, c.Line)
		if c.HTMLURL == "" {
			// No URL to link, so name the location instead.
			fmt.Fprintf(&b, "- %s\n", anchor)
			continue
		}
		fmt.Fprintf(&b, "- [%s](%s)\n", anchor, c.HTMLURL)
	}
	return b.String()
}

// recordFinding dedupes by comment ID, because reposting a finding in the same
// run updates one comment and the verdict must count it once.
func (s *Set) recordFinding(number int, c ReviewComment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findings == nil {
		s.findings = make(map[int][]ReviewComment)
	}
	for i, existing := range s.findings[number] {
		if existing.ID == c.ID {
			s.findings[number][i] = c
			return
		}
	}
	s.findings[number] = append(s.findings[number], c)
}

// currentFindings are the findings this run posted or updated, excluding older
// marked comments, so an approval never links a finding the author has fixed.
func (s *Set) currentFindings(number int) []ReviewComment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ReviewComment(nil), s.findings[number]...)
}

// takeFindings returns and forgets the findings, so a second review through the
// same Set cannot link the first review's fixed findings.
func (s *Set) takeFindings(number int) []ReviewComment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.findings[number]
	delete(s.findings, number)
	return out
}

func (t submitReviewTool) Invoke(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Event      string `json:"event"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", err
	}
	number := t.s.cfg.Number
	switch in.Event {
	case "COMMENT", "REQUEST_CHANGES":
	case "APPROVE":
		if !t.s.cfg.AllowApprove {
			return "", ErrApproveNotEnabled
		}
	default:
		return "", fmt.Errorf("github: unknown review event %q", in.Event)
	}

	pr, err := t.s.client.pullRequest(ctx, number)
	if err != nil {
		return "", err
	}
	// Both logins come from GitHub, so the model cannot misreport the author and
	// an unset SelfLogin cannot disable the check.
	if in.Event == "APPROVE" {
		self, serr := t.s.selfLogin(ctx)
		if serr != nil {
			return "", serr
		}
		if sameLogin(pr.AuthorLogin, self) {
			return "", ErrSelfApproval
		}
	}
	// Coverage gates APPROVE (see ErrIncompleteDiff) and whether a missing
	// finding counts as fixed in resolveStaleThreads. For other verdicts a
	// coverage that could not be read must not block the verdict; nothing is
	// retracted, and coverageErr says so in the result.
	coverage := t.s.diffCoverage(ctx, pr)
	var complete bool
	var coverageErr error
	switch coverage.Outcome() {
	case verdict.Passed:
		complete = true
	case verdict.Failed:
		if in.Event == "APPROVE" {
			return "", fmt.Errorf("%w: %s", ErrIncompleteDiff, coverage.Reason())
		}
	case verdict.Unknown:
		if in.Event == "APPROVE" {
			return "", coverage.Err()
		}
		coverageErr = coverage.Err()
	}
	// Checked after the refusals above, so a model denied APPROVE is told that
	// and does not rewrite prose to get past it. Catching it here avoids GitHub's
	// opaque 422 for an empty body.
	if strings.TrimSpace(in.Conclusion) == "" {
		return "", errors.New("github: a verdict needs a one-sentence conclusion")
	}

	posted := t.s.currentFindings(number)
	if err := t.s.client.submitReview(ctx, number, pr.HeadSHA, in.Event, verdictBody(in.Conclusion, posted)); err != nil {
		// Findings are kept so a retry still links them.
		return "", err
	}
	t.s.takeFindings(number)

	// Runs after the verdict, so a failure here cannot lose the verdict.
	resolved, rerr := t.s.resolveStaleThreads(ctx, number, complete, posted)
	if coverageErr != nil {
		rerr = fmt.Errorf("the diff could not be read, so no stale conversation was retracted: %w", coverageErr)
	}
	out := fmt.Sprintf("submitted %s on #%d, linking %d finding(s)", in.Event, number, len(posted))
	if resolved > 0 {
		out += fmt.Sprintf(", resolved %d stale thread(s)", resolved)
	}
	if rerr != nil {
		// The verdict is in, so report the miss in the result instead of failing.
		out += fmt.Sprintf("; could not resolve every stale thread: %v", rerr)
	}
	return out, nil
}
