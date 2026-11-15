package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	acceptJSON = "application/vnd.github+json"
	apiVersion = "2022-11-28"

	// maxErrorBody caps how much of a failed response is read into an error message.
	maxErrorBody = 4 << 10
	// perPage is the page size for paginated list endpoints.
	perPage = 100
	// maxPages bounds pagination so a pathological response cannot loop forever.
	maxPages = 20
)

type client struct {
	cfg  Config
	auth tokenSource
}

// PullRequest is the subset of a pull request a review needs.
type PullRequest struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	State       string `json:"state"`
	Draft       bool   `json:"draft"`
	HeadSHA     string `json:"-"`
	AuthorLogin string `json:"-"`

	// ChangedFiles, Additions and Deletions are GitHub's totals, independent of
	// how much diff was fetched, so they are safe to gate a verdict on.
	ChangedFiles int `json:"changed_files"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`

	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ChangedLines is additions plus deletions.
func (p PullRequest) ChangedLines() int { return p.Additions + p.Deletions }

// ChangedFile is one file in a diff. A binary or very large file has no patch.
type ChangedFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`

	// PatchTruncated reports that Patch was cut to Config.MaxPatchBytes.
	PatchTruncated bool `json:"patch_truncated,omitempty"`
}

// ReviewComment is an inline comment on a pull request's diff.
type ReviewComment struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`

	// HTMLURL lets a verdict link a finding instead of repeating it.
	HTMLURL string `json:"html_url"`

	// User is the author. A marker does not prove authorship, since anyone can
	// copy one.
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// do decodes the JSON response into out; a nil out discards it. A non-2xx
// response becomes a statusError.
func (c *client) do(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("github: encoding request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	tok, err := c.auth.installationToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Expose())
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, redactURL(url), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp, method+" "+redactURL(url))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github: decoding response: %w", err)
	}
	return nil
}

func (c *client) prURL(number int, suffix string) string {
	return fmt.Sprintf("%s/repos/%s/%s/pulls/%d%s",
		c.cfg.APIBase, c.cfg.Owner, c.cfg.Repo, number, suffix)
}

func (c *client) pullRequest(ctx context.Context, number int) (PullRequest, error) {
	var pr PullRequest
	if err := c.do(ctx, http.MethodGet, c.prURL(number, ""), nil, &pr); err != nil {
		return PullRequest{}, err
	}
	pr.HeadSHA, pr.AuthorLogin = pr.Head.SHA, pr.User.Login
	return pr, nil
}

// changedFiles follows pagination up to Config.MaxFiles and cuts each patch to
// Config.MaxPatchBytes.
func (c *client) changedFiles(ctx context.Context, number int) ([]ChangedFile, bool, error) {
	var all []ChangedFile
	url := fmt.Sprintf("%s?per_page=%d", c.prURL(number, "/files"), perPage)
	for page := 0; page < maxPages && url != ""; page++ {
		var batch []ChangedFile
		next, err := c.doPaged(ctx, url, &batch)
		if err != nil {
			return nil, false, err
		}
		all = append(all, batch...)
		if len(all) >= c.cfg.MaxFiles {
			return truncatePatches(all[:c.cfg.MaxFiles], c.cfg.MaxPatchBytes), true, nil
		}
		url = next
	}
	return truncatePatches(all, c.cfg.MaxPatchBytes), false, nil
}

// doPaged returns the next page's URL, or "" on the last page.
func (c *client) doPaged(ctx context.Context, url string, out any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	tok, err := c.auth.installationToken(ctx)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Expose())
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github: GET %s: %w", redactURL(url), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", statusError(resp, "GET "+redactURL(url))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", fmt.Errorf("github: decoding response: %w", err)
	}
	return c.nextPage(resp.Header.Get("Link"))
}

// linkNext matches the next-page URL in a Link header. The URL may carry neither
// delimiter, so a nested bracket cannot be swallowed into the captured address.
var linkNext = regexp.MustCompile(`<([^<>]+)>;\s*rel="next"`)

func nextLink(header string) string {
	if m := linkNext.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	return ""
}

// nextPage refuses a next link off the API origin, because the responder picks
// the link and the request carries our token. The egress policy would not
// catch it, since it refuses private addresses and allows public ones.
func (c *client) nextPage(header string) (string, error) {
	raw := nextLink(header)
	if raw == "" {
		return "", nil
	}
	next, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("github: unparsable pagination link: %w", err)
	}
	base, err := url.Parse(c.cfg.APIBase)
	if err != nil {
		return "", fmt.Errorf("github: unparsable API base: %w", err)
	}
	if next.Scheme != base.Scheme || next.Host != base.Host {
		return "", fmt.Errorf("github: pagination link %q leaves the API host %q",
			redactURL(raw), base.Host)
	}
	return raw, nil
}

// truncatePatches cuts each patch to at most limit bytes after the last newline
// in range, keeping the newline, so the model never mistakes half a line for a
// whole one. Keeping it also handles a patch whose only newline is byte 0. A
// patch with no newline in range is cut at limit. Either way PatchTruncated is
// set.
func truncatePatches(files []ChangedFile, limit int) []ChangedFile {
	for i := range files {
		p := files[i].Patch
		if len(p) <= limit {
			continue
		}
		cut := p[:limit]
		if nl := strings.LastIndexByte(cut, '\n'); nl >= 0 {
			cut = cut[:nl+1]
		}
		files[i].Patch = cut
		files[i].PatchTruncated = true
	}
	return files
}

// reviewComments fails when it runs out of pages, because a finding missing
// from a short list is never restated and so gets retracted.
func (c *client) reviewComments(ctx context.Context, number int) ([]ReviewComment, error) {
	var all []ReviewComment
	url := fmt.Sprintf("%s?per_page=%d", c.prURL(number, "/comments"), perPage)
	for range maxPages {
		var batch []ReviewComment
		next, err := c.doPaged(ctx, url, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if next == "" {
			return all, nil
		}
		url = next
	}
	return nil, fmt.Errorf("github: more than %d pages of review comments", maxPages)
}

func (c *client) createReviewComment(ctx context.Context, number int, headSHA string, f Finding) (ReviewComment, error) {
	in := map[string]any{
		"body":      f.render(),
		"commit_id": headSHA,
		"path":      f.Path,
		"line":      f.Line,
		"side":      "RIGHT",
	}
	var out ReviewComment
	if err := c.do(ctx, http.MethodPost, c.prURL(number, "/comments"), in, &out); err != nil {
		return ReviewComment{}, err
	}
	// A fake or proxy may not echo the comment, so fill in the known
	// coordinates.
	out.Path, out.Line, out.Body = f.Path, f.Line, f.render()
	return out, nil
}

func (c *client) updateReviewComment(ctx context.Context, id int64, f Finding) (ReviewComment, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/comments/%d",
		c.cfg.APIBase, c.cfg.Owner, c.cfg.Repo, id)
	var out ReviewComment
	if err := c.do(ctx, http.MethodPatch, url, map[string]any{"body": f.render()}, &out); err != nil {
		return ReviewComment{}, err
	}
	out.ID, out.Path, out.Line, out.Body = id, f.Path, f.Line, f.render()
	return out, nil
}

// submitReview does no authorization; the caller has checked the event.
func (c *client) submitReview(ctx context.Context, number int, headSHA, event, body string) error {
	in := map[string]any{"event": event, "commit_id": headSHA}
	if body != "" {
		in["body"] = body
	}
	return c.do(ctx, http.MethodPost, c.prURL(number, "/reviews"), in, nil)
}

// statusError includes a bounded prefix of the body so a 422 explains itself.
func statusError(resp *http.Response, what string) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		return fmt.Errorf("github: %s: %s", what, resp.Status)
	}
	return fmt.Errorf("github: %s: %s: %s", what, resp.Status, msg)
}

// redactURL strips the query string so a token in it never reaches a log.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
