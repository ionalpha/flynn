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

// fingerprint is the finding's stable identity: its location and the rule that
// found it. The body is excluded on purpose, so rewording a finding updates the
// existing comment instead of orphaning it.
//
// It is a content fingerprint over public coordinates, not a secret and not a
// credential: a path, a line number, and a rule name, all of which are printed in
// the comment it identifies. SHA-256 is the right tool for that. It was called
// "key", which read as a cryptographic key and had a security scanner reporting
// it as a password hashed with a function too cheap to hash passwords with.
func (f Finding) fingerprint() string {
	sum := sha256.Sum256([]byte(f.Path + "\x00" + strconv.Itoa(f.Line) + "\x00" + f.Rule))
	return hex.EncodeToString(sum[:6])
}

// marker is the invisible identity tag embedded in the finding's comment body.
func (f Finding) marker() string { return markerPrefix + f.fingerprint() + " -->" }

// ruleTagPrefix opens the HTML comment that carries a finding's rule. Like the marker
// it renders invisibly, so the rule stays out of the author's view: a reader sees the
// defect and the failure it causes, not an internal slug shouted in bold above them. A
// re-review still reads the rule back from here to repost the same finding in place.
const ruleTagPrefix = "<!-- flynn-rule:"

// render builds the comment body for a finding. The marker and the rule are invisible
// HTML comments, so the visible comment is the defect and the failure it causes and
// nothing else; a re-review recovers both from the body to reconcile in place. See
// bodyParts, which reads them back.
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

// validate reports why a finding cannot be posted, or nil when it can.
func (f Finding) validate() error {
	switch {
	case f.Path == "":
		return errors.New("github: finding has no path")
	case f.Line <= 0:
		return fmt.Errorf("github: finding on %s has no line", f.Path)
	case f.Rule == "":
		return fmt.Errorf("github: finding at %s:%d has no rule", f.Path, f.Line)
	case strings.Contains(f.Rule, "-->") || strings.ContainsRune(f.Rule, '\n'):
		// The rule renders inside an HTML comment, so a "-->" or a newline in it would
		// close the tag early and detach the finding from its own claim. A rule is a short
		// slug; reject anything that cannot live in the tag rather than post a broken one.
		return fmt.Errorf("github: finding at %s:%d has a rule that cannot be embedded: %q", f.Path, f.Line, f.Rule)
	case strings.TrimSpace(f.Summary) == "":
		return fmt.Errorf("github: finding at %s:%d has no summary", f.Path, f.Line)
	case strings.TrimSpace(f.Failure) == "":
		return fmt.Errorf("github: finding at %s:%d has no failure scenario", f.Path, f.Line)
	}
	return nil
}

// PostedFinding is a finding already on the pull request, handed back so the reviewer
// can decide whether its defect is still there. Path, Line, and Rule regenerate the
// marker, so reposting updates the existing comment rather than opening a second
// conversation about the same defect. Summary and Failure are the claim the reviewer
// made last time: without them it would have only a hash of the location and would
// restate the finding on faith instead of re-reading what it said and checking the
// current diff against it.
type PostedFinding struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Rule    string `json:"rule"`
	Summary string `json:"summary,omitempty"`
	Failure string `json:"failure,omitempty"`
}

// bodyParts recovers the rule, summary, and failure scenario from a finding's rendered
// comment body. render wrote them as:
//
//	<marker>
//	<rule tag>
//	<summary>
//
//	<failure>
//
// with the marker and rule as invisible HTML comments, so the reviewer can re-read the
// claim it posted last time rather than reconstruct it from the location hash. A body
// without the rule tag (a human's comment, or a comment from before this format) comes
// back all "".
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

	// DiffComplete reports whether every changed file and every patch reached the
	// review intact. When it is false the review is working from a partial picture,
	// and an APPROVE verdict will be refused.
	DiffComplete bool `json:"diff_complete"`

	// PostedFindings are the findings this reviewer already has on the pull request,
	// each carrying the claim it made: its rule, summary, and failure scenario, not
	// just the location. The reviewer re-reads that claim and checks the current diff
	// against it, reposting the ones whose defect is still there and dropping the ones
	// the change has fixed. Handed only a location hash it could not tell the two apart
	// and would restate stale findings on faith.
	PostedFindings []PostedFinding `json:"posted_findings,omitempty"`
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

func (t prFetchTool) Invoke(ctx context.Context, _ json.RawMessage) (string, error) {
	// The pull request is bound to the run, not read from the tool input. A review that
	// mentions another pull request's number in its own diff cannot fetch (or later
	// comment on) that pull request instead of the one it was invoked for.
	number := t.s.cfg.Number
	pr, err := t.s.client.pullRequest(ctx, number)
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
	files, truncated, err := t.s.client.changedFiles(ctx, number)
	if err != nil {
		return "", err
	}
	existing, err := t.s.client.reviewComments(ctx, number)
	if err != nil {
		return "", err
	}
	// A marker says which finding a comment stands for, never who wrote it: it is plain
	// text in a body anyone can copy. Without an identity to check the author against,
	// the reviewer cannot tell its own findings from a maintainer's, and reports none
	// rather than inviting itself to stand behind or retract somebody else's comment.
	self, err := t.s.selfLogin(ctx)
	if err != nil {
		return "", err
	}
	posted := make([]PostedFinding, 0, len(existing))
	for _, c := range existing {
		if markerIn(c.Body) == "" || !sameLogin(c.User.Login, self) {
			continue
		}
		// GitHub reports line as null for a comment whose anchor no longer exists in the
		// diff, which decodes to zero. Such a finding cannot be restated where it was
		// made, and offering it back with a line of zero would have the reviewer repost
		// something the finding validator refuses. It is left out: GitHub already reports
		// the thread as outdated, and an outdated thread is retracted on that evidence
		// rather than on the reviewer's silence.
		if c.Line <= 0 {
			continue
		}
		rule, summary, failure := bodyParts(c.Body)
		posted = append(posted, PostedFinding{
			Path: c.Path, Line: c.Line, Rule: rule, Summary: summary, Failure: failure,
		})
	}
	// Ordered so a pull request nothing has happened to reads the same way twice. Two
	// findings may share a path and a line and differ only in their rule, and each keys
	// a conversation of its own. The rule breaks that tie: without it sort.Slice, which
	// is not stable, hands the reviewer a different list on every run.
	sort.Slice(posted, func(i, j int) bool {
		if posted[i].Path != posted[j].Path {
			return posted[i].Path < posted[j].Path
		}
		if posted[i].Line != posted[j].Line {
			return posted[i].Line < posted[j].Line
		}
		return posted[i].Rule < posted[j].Rule
	})

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

// Invoke posts the findings inline and nothing else.
//
// A review used to also keep a summary comment on the pull request's main thread,
// restating what the inline comments already said and what the verdict says again
// underneath. Three messages for one defect reads as a chatty bot and buries the
// comment that mattered. A finding belongs on its line; the conclusion belongs to the
// verdict; the main thread gets neither.
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
	// Validate every finding before posting any, so a malformed batch does not leave
	// the pull request half-reviewed.
	for _, f := range in.Findings {
		if err := f.validate(); err != nil {
			return "", err
		}
	}

	// The pull request is the run's bound one, never a number the model supplied.
	created, updated, err := t.reconcileFindings(ctx, t.s.cfg.Number, in.Findings)
	if err != nil {
		return "", err
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
	// Only the reviewer's own comments are reconciled. The marker says which finding a
	// comment stands for, not who wrote it, and a maintainer quoting a finding back has
	// quoted the key to their own comment. Matching on the marker alone would rewrite
	// their words in place. Without an identity to check the author against, nothing is
	// adopted: a new comment is posted rather than someone else's overwritten.
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

// verdictBody builds the review body: the reviewer's one-sentence conclusion, then a
// link to each finding it has posted inline on this pull request.
//
// The list is built here rather than written by the model, which is the point. GitHub
// requires a body on a COMMENT or REQUEST_CHANGES review, and a model handed an empty
// text field fills it, restating on the main thread what the inline comments already
// say. Linking states what the verdict covers without saying any of it twice, and a
// list the tool constructs cannot grow prose.
//
// Findings are ordered by file and line so the list reads in the order a reader walks
// the diff, not in the order the reviewer happened to find them.
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
			// A comment GitHub gave no address to cannot be linked; naming it still
			// tells the reader where to look, which is better than dropping it.
			fmt.Fprintf(&b, "- %s\n", anchor)
			continue
		}
		fmt.Fprintf(&b, "- [%s](%s)\n", anchor, c.HTMLURL)
	}
	return b.String()
}

// recordFinding remembers a finding this run posted or updated, so the verdict can
// link exactly what this review had to say.
//
// It is keyed by the comment the finding lives on. A reviewer may call the comment
// tool more than once in a run, re-proposing a finding it already posted; that updates
// the one comment rather than adding a second, and the verdict must count it once. A
// list that grew per call would report "2 findings" and link the same line twice.
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

// currentFindings are the findings this run posted or updated.
//
// It is deliberately not "every comment on the pull request carrying our marker". A
// finding the author has since fixed is not re-proposed, so it is not in this list,
// and a verdict that listed it would contradict itself: an approval whose body links
// an obsolete finding says both that nothing blocks the merge and that something does.
// Comments from earlier rounds stay on the pull request until they are resolved; what
// they do not do is speak for this verdict.
func (s *Set) currentFindings(number int) []ReviewComment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ReviewComment(nil), s.findings[number]...)
}

// takeFindings returns the findings recorded for a pull request and forgets them.
//
// Submitting a verdict ends a review, so what it found belongs to the review that just
// ended. A host that reviews the same pull request twice through one Set starts the
// second review with nothing recorded, and cannot link a finding the first review made
// and the author has since fixed.
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
	// The verdict lands on the run's bound pull request, never a number the model chose.
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
	// The self-approval check reads the live author rather than trusting the caller, so
	// a model cannot talk its way past it by misreporting who opened the change. The
	// reviewer's own login is resolved from the credential, so the check cannot be
	// switched off by leaving it unconfigured.
	if in.Event == "APPROVE" {
		self, serr := t.s.selfLogin(ctx)
		if serr != nil {
			return "", serr
		}
		if sameLogin(pr.AuthorLogin, self) {
			return "", ErrSelfApproval
		}
	}
	// Whether the whole diff reached this review. It gates the approval below, and it
	// gates whether a finding's absence is evidence the defect is gone (see
	// resolveStaleThreads).
	//
	// An APPROVE asserts the change was reviewed. If the diff was truncated on the way
	// in, that assertion is false, and it is the assertion a signed verdict would go on
	// to attest. Blocking verdicts stay available on partial evidence: seeing one real
	// defect is enough to say no, but seeing most of a diff is never enough to say yes.
	// No configuration relaxes this.
	//
	// For every other verdict the coverage read is advisory, and its failure must not
	// cost the review its verdict. A transient error on the changed-files endpoint would
	// otherwise stop a reviewer that had found a real defect from ever saying so, in
	// service of housekeeping that runs after the verdict anyway. An unreadable diff is
	// treated as incomplete, so nothing is resolved on the strength of it.
	var complete bool
	var coverageErr error
	if in.Event == "APPROVE" {
		ok, why, err := t.s.diffCoverage(ctx, pr)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrIncompleteDiff, why)
		}
		complete = true
	} else {
		// The error is kept, not discarded. It leaves the diff unread, which skips thread
		// resolution below, and a reviewer that said nothing about that would look like it
		// had chosen to leave its stale conversations open on a repository whose merge is
		// blocked by them.
		complete, _, coverageErr = t.s.diffCoverage(ctx, pr)
		if coverageErr != nil {
			complete = false
		}
	}
	// Validated last, after every refusal above. A reviewer denied the authority to
	// approve must be told that, not that its sentence was missing: the weaker error
	// would send a model off rewriting prose to get past a gate that is not about
	// prose. GitHub refuses a COMMENT or REQUEST_CHANGES review with no body, so an
	// absent conclusion is caught here rather than as a 422 the model cannot read.
	if strings.TrimSpace(in.Conclusion) == "" {
		return "", errors.New("github: a verdict needs a one-sentence conclusion")
	}

	// The body links the findings already on the pull request, so the verdict says what
	// it covers without restating any of it.
	posted := t.s.currentFindings(number)
	if err := t.s.client.submitReview(ctx, number, pr.HeadSHA, in.Event, verdictBody(in.Conclusion, posted)); err != nil {
		// The record survives a failed submission, so a retry still links what this
		// review found rather than submitting a verdict that cites nothing.
		return "", err
	}
	// The verdict landed, so the review is over and its findings belong to it alone.
	t.s.takeFindings(number)

	// Close the conversations this review no longer stands behind. It runs after the
	// verdict, so a failure here cannot cost the review its verdict; a repository that
	// requires thread resolution would otherwise be wedged by a reviewer that had
	// nothing left to say.
	resolved, rerr := t.s.resolveStaleThreads(ctx, number, complete, posted)
	if coverageErr != nil {
		// Resolution never ran: the diff could not be read, so a finding's absence proved
		// nothing about whether it was fixed.
		rerr = fmt.Errorf("the diff could not be read, so no stale conversation was retracted: %w", coverageErr)
	}
	out := fmt.Sprintf("submitted %s on #%d, linking %d finding(s)", in.Event, number, len(posted))
	if resolved > 0 {
		out += fmt.Sprintf(", resolved %d stale thread(s)", resolved)
	}
	if rerr != nil {
		// The verdict is in. Say what did not happen rather than failing the review, and
		// say it in the tool's result so the reviewer's own record carries it.
		out += fmt.Sprintf("; could not resolve every stale thread: %v", rerr)
	}
	return out, nil
}
