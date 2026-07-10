package github

import (
	"context"
	"fmt"
	"strings"
)

// Resolving a thread is a write the model never makes.
//
// A reviewer that could resolve conversations on request could be argued into closing
// the one finding that mattered. So there is no resolve tool: the verdict resolves,
// deterministically, from what the run actually found. The rule below is the whole of
// the reviewer's authority to close a conversation, and it is readable in one screen
// on purpose.

// sameLogin reports whether two logins name the same actor.
//
// The two GitHub APIs disagree about a bot's name. REST reports an App's login with
// the suffix ("vouchbot[bot]"), which is what a reviewer is configured with and what
// appears on a pull request. GraphQL reports the Bot actor's login without it
// ("vouchbot"). Comparing them literally makes a reviewer a stranger to its own
// conversations, and it resolves nothing, silently: every thread simply looks like
// somebody else's. The suffix is not part of the identity, so it is not compared.
func sameLogin(a, b string) bool {
	return strings.TrimSuffix(a, "[bot]") == strings.TrimSuffix(b, "[bot]")
}

// resolvableThread reports whether the reviewer may close this thread, and why not
// when it may not. The reason is returned for the error path and for tests: a rule
// this consequential should be able to say which clause refused.
//
// found is the set of markers the run posted or updated. A finding still present is a
// finding still open, whatever GitHub thinks about the diff hunk moving.
//
// self is the reviewer's own login. The marker alone does not establish who wrote a
// comment: it is plain text in a body anyone can read and copy, so a person quoting a
// finding back at the reviewer would be quoting the key to their own conversation. The
// author is the fact; the marker only says which finding the thread belongs to.
func resolvableThread(t ReviewThread, self string, found map[string]bool) (bool, string) {
	switch {
	case self == "":
		// Without an identity there is nothing to compare the author against, so the
		// reviewer cannot tell its own conversation from anyone else's, and closes none.
		return false, "the reviewer has no identity to check the author against"
	case !sameLogin(t.Author, self):
		return false, "opened by someone else"
	case t.Marker == "":
		// The reviewer's own comment, but not one of its findings.
		return false, "not a finding"
	case t.Resolved:
		return false, "already resolved"
	case t.Truncated:
		// More comments than were read, so whether anybody replied is unknown. Silence is
		// only evidence when the whole conversation was heard.
		return false, "the conversation is longer than one page"
	case t.Participants > 1:
		// Someone replied. A bot closing a conversation under a person talking in it is
		// worse than a stale comment: it destroys the reply's context and reads as the
		// bot overruling them.
		return false, "someone replied"
	case found[t.Marker]:
		return false, "the finding was raised again in this review"
	case t.Outdated:
		// GitHub's own judgement: the diff hunk this thread is anchored to no longer
		// matches the head. The code it objected to is gone.
		return true, ""
	default:
		// The reviewer read the whole diff and did not raise this finding again, so the
		// defect it described is no longer there. The caller has already established that
		// the diff arrived complete; without that, absence would mean nothing.
		//
		// This reads a silence, so the silence has to mean something. The reviewer's
		// standing instruction, and both tool descriptions, tell it to post again every
		// finding whose defect is still present, and the comment tool updates the existing
		// comment rather than opening a second conversation. Without that contract, a
		// reviewer told not to repeat itself would say nothing about a live defect and
		// watch its own objection be retracted.
		return true, ""
	}
}

// resolveStaleThreads closes the reviewer's own threads whose findings are gone, and
// reports how many it closed.
//
// diffComplete gates the whole operation. A finding can be absent from a review
// because the author fixed it, or because the diff arrived truncated and the reviewer
// never saw the code. Those are indistinguishable from here, and resolving on absence
// alone would eventually close a live defect on a large pull request. This is the same
// invariant github_submit_review enforces before it will approve, for the same reason:
// a claim about a diff nobody read is a false claim.
// posted is the set of findings this review raised, passed in rather than read back
// from the Set: submitting the verdict ends the review and takes its record with it.
func (s *Set) resolveStaleThreads(ctx context.Context, number int, diffComplete bool, posted []ReviewComment) (int, error) {
	// Without a configured identity the reviewer cannot tell its own conversation from
	// anyone else's, so it closes none and does not spend a request finding that out.
	if !diffComplete || s.cfg.SelfLogin == "" {
		return 0, nil
	}
	threads, err := s.client.reviewThreads(ctx, number)
	if err != nil {
		return 0, fmt.Errorf("read review threads: %w", err)
	}

	found := make(map[string]bool)
	for _, c := range posted {
		if m := markerIn(c.Body); m != "" {
			found[m] = true
		}
	}

	var resolved, minimized int
	for _, t := range threads {
		if ok, _ := resolvableThread(t, s.cfg.SelfLogin, found); !ok {
			continue
		}
		// Resolving needs write access to the repository. A reviewer granted only
		// pull_requests:write cannot resolve, and GitHub says so per thread rather than
		// leaving it to be discovered as a 403. Where it may not resolve, it retracts the
		// finding the way it can: it folds its own comment away, marked outdated. That
		// leaves the conversation on the record and leaves it unresolved, so a repository
		// requiring resolution still waits for someone who can.
		if t.CanResolve {
			if err := s.client.resolveThread(ctx, t.ID); err != nil {
				// Report the failure rather than swallowing it. An unresolved thread blocks
				// the merge on any repository that requires thread resolution, so a reviewer
				// that quietly failed to close its own conversations would look like it had
				// wedged the pull request for no reason.
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
