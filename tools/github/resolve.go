package github

import (
	"context"
	"fmt"
	"strings"
)

// sameLogin ignores a "[bot]" suffix, because REST reports an App's login with
// it ("vouchbot[bot]") and GraphQL without it ("vouchbot"). A literal compare
// would make every one of the reviewer's threads look like someone else's.
func sameLogin(a, b string) bool {
	return strings.TrimSuffix(a, "[bot]") == strings.TrimSuffix(b, "[bot]")
}

// resolvableThread is the reviewer's whole authority to close a thread. No tool
// resolves on request, so the model cannot be argued into closing a live
// finding. The returned reason names the clause that refused.
//
// found holds the markers this run posted or updated; those stay open even if
// GitHub marks the hunk outdated. The author is checked against self because
// anyone can copy a marker.
func resolvableThread(t ReviewThread, self string, found map[string]bool) (bool, string) {
	switch {
	case self == "":
		return false, "the reviewer has no identity to check the author against"
	case !sameLogin(t.Author, self):
		return false, "opened by someone else"
	case t.Marker == "":
		// The reviewer's own comment, but not one of its findings.
		return false, "not a finding"
	case t.Resolved:
		return false, "already resolved"
	case t.Truncated:
		// An unread comment may be a reply.
		return false, "the conversation is longer than one page"
	case t.Participants > 1:
		// Closing under a person's reply hides its context.
		return false, "someone replied"
	case found[t.Marker]:
		return false, "the finding was raised again in this review"
	case t.Outdated:
		// GitHub says the anchored hunk no longer matches the head.
		return true, ""
	default:
		// The caller checked the diff was complete and the finding was not
		// reposted, so the defect is gone. This relies on the tool descriptions
		// telling the reviewer to repost every live finding.
		return true, ""
	}
}

// resolveStaleThreads closes the reviewer's threads whose findings are gone and
// returns the count. It does nothing unless diffComplete, because a finding
// missing from a truncated diff may still be live (the rule behind
// ErrIncompleteDiff). posted is passed in because the verdict has already
// taken the Set's record.
func (s *Set) resolveStaleThreads(ctx context.Context, number int, diffComplete bool, posted []ReviewComment) (int, error) {
	if !diffComplete {
		return 0, nil
	}
	self, err := s.selfLogin(ctx)
	if err != nil {
		return 0, err
	}
	threads, terr := s.client.reviewThreads(ctx, number)
	if terr != nil {
		return 0, fmt.Errorf("read review threads: %w", terr)
	}

	found := make(map[string]bool)
	for _, c := range posted {
		if m := markerIn(c.Body); m != "" {
			found[m] = true
		}
	}

	var resolved, minimized int
	for _, t := range threads {
		if ok, _ := resolvableThread(t, self, found); !ok {
			continue
		}
		// Resolving needs repository write access, which pull_requests:write lacks;
		// GitHub reports it per thread. Without it the comments are minimized as
		// outdated, and the thread stays unresolved for someone who can.
		if t.CanResolve {
			if err := s.client.resolveThread(ctx, t.ID); err != nil {
				// Reported because an unresolved thread can block the merge.
				return resolved, fmt.Errorf("resolve thread: %w", err)
			}
			resolved++
			continue
		}
		for _, id := range t.CommentNodeIDs {
			if err := s.client.minimizeComment(ctx, id); err != nil {
				return resolved, fmt.Errorf("minimize comment: %w", err)
			}
		}
		minimized++
	}
	if minimized > 0 {
		return resolved, fmt.Errorf(
			"%d stale finding(s) were folded away but left unresolved: resolving a conversation needs write access to the repository, and this reviewer has none",
			minimized,
		)
	}
	return resolved, nil
}

// selfLogin returns Config.SelfLogin, or asks GitHub once and caches the answer.
// Every authorship check depends on it.
func (s *Set) selfLogin(ctx context.Context) (string, error) {
	if s.cfg.SelfLogin != "" {
		return s.cfg.SelfLogin, nil
	}
	s.mu.Lock()
	cached := s.self
	s.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	login, err := s.client.viewerLogin(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve the reviewer's own login: %w", err)
	}
	s.mu.Lock()
	s.self = login
	s.mu.Unlock()
	return login, nil
}
