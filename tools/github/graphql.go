package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// GraphQL is reached only for what REST cannot do. Resolving a review thread is the
// one such operation a reviewer needs: there is no REST endpoint for it, and a thread
// id is not a comment id, so the threads must be read from the same API that resolves
// them.

// graphqlURL derives the GraphQL endpoint from the REST API root.
//
// On github.com the two sit side by side: https://api.github.com/graphql. A GitHub
// Enterprise appliance serves REST under /api/v3 and GraphQL under /api/graphql, not
// under /api/v3/graphql, so the version segment is replaced rather than extended. Any
// other base has /graphql appended, which is what a test server wants.
func graphqlURL(apiBase string) string {
	base := strings.TrimSuffix(apiBase, "/")
	if strings.HasSuffix(base, "/api/v3") {
		return strings.TrimSuffix(base, "/v3") + "/graphql"
	}
	return base + "/graphql"
}

// graphqlError is one error GitHub reports for a GraphQL request.
type graphqlError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// graphql issues a GraphQL request and decodes `data` into out.
//
// A GraphQL request that fails answers 200 with an `errors` array and a null or
// partial `data`, so the HTTP status alone says nothing. Treating a 200 as success
// here would let a permission failure or a bad query read as "no threads to resolve",
// and the reviewer would silently stop resolving anything.
func (c *client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	in := map[string]any{"query": query}
	if len(vars) > 0 {
		in["variables"] = vars
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphqlError  `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, graphqlURL(c.cfg.APIBase), in, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("github: graphql: %s", strings.Join(msgs, "; "))
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return errors.New("github: graphql: response carried no data")
	}
	return json.Unmarshal(envelope.Data, out)
}

// ReviewThread is a conversation on a line of a pull request's diff.
type ReviewThread struct {
	// ID is the GraphQL node id, which is what resolveReviewThread takes. It is not
	// the REST comment id.
	ID string

	// Resolved reports whether the thread is already closed.
	Resolved bool

	// Outdated reports GitHub's own judgement that the diff hunk this thread is
	// anchored to no longer matches the head of the pull request.
	Outdated bool

	// Marker keys the thread to the finding that opened it, taken from the body of
	// its first comment. A thread a human opened carries none.
	Marker string

	// Author is the login that opened the thread.
	Author string

	// Participants counts the distinct logins that have commented. More than one means
	// somebody replied.
	Participants int

	// CanResolve reports whether the authenticated identity may close this thread.
	// Resolving a conversation needs write access to the repository, which a reviewer
	// holding only pull_requests:write does not have. A reviewer that can never push is
	// therefore a reviewer that can never resolve, and must retract its finding another
	// way. GitHub answers this per thread, so the capability is read rather than guessed.
	CanResolve bool

	// CommentNodeIDs are the GraphQL ids of the thread's comments, in order. They are
	// what minimizeComment takes, and minimizing is the retraction available to a
	// reviewer that may not resolve.
	CommentNodeIDs []string

	// Truncated reports that the thread carries more comments than were read. Whether
	// anybody replied is then unknown, and a reviewer that guessed would eventually
	// retract a finding somebody was still arguing with.
	Truncated bool
}

// reviewThreadsQuery reads a page of threads on a pull request. It is paginated: a
// long-running pull request accumulates conversations, and a reviewer that read only
// the first page would leave every thread past it open forever, blocking the merge on
// any repository that requires resolution. The page cursor is threads; 100 comments
// per thread is a bound on participation, not on the pull request.
const reviewThreadsQuery = `query($owner:String!,$repo:String!,$number:Int!,$after:String){
  repository(owner:$owner,name:$repo){
    pullRequest(number:$number){
      reviewThreads(first:100, after:$after){
        pageInfo{ hasNextPage endCursor }
        nodes{
          id
          isResolved
          isOutdated
          viewerCanResolve
          comments(first:100){ pageInfo{ hasNextPage } nodes{ id body author{ login } } }
        }
      }
    }
  }
}`

// maxThreadPages bounds pagination so a pathological response cannot loop forever,
// the same guard the REST pagination uses.
const maxThreadPages = 20

// reviewThreads lists every review thread on a pull request, following pagination.
func (c *client) reviewThreads(ctx context.Context, number int) ([]ReviewThread, error) {
	var out []ReviewThread
	var after *string

	for range maxThreadPages {
		var resp struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID               string `json:"id"`
							IsResolved       bool   `json:"isResolved"`
							IsOutdated       bool   `json:"isOutdated"`
							ViewerCanResolve bool   `json:"viewerCanResolve"`
							Comments         struct {
								PageInfo struct {
									HasNextPage bool `json:"hasNextPage"`
								} `json:"pageInfo"`
								Nodes []struct {
									ID     string `json:"id"`
									Body   string `json:"body"`
									Author struct {
										Login string `json:"login"`
									} `json:"author"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": c.cfg.Owner, "repo": c.cfg.Repo, "number": number}
		if after != nil {
			vars["after"] = *after
		}
		if err := c.graphql(ctx, reviewThreadsQuery, vars, &resp); err != nil {
			return nil, err
		}

		threads := resp.Repository.PullRequest.ReviewThreads
		for _, n := range threads.Nodes {
			t := ReviewThread{
				ID: n.ID, Resolved: n.IsResolved, Outdated: n.IsOutdated,
				CanResolve: n.ViewerCanResolve, Truncated: n.Comments.PageInfo.HasNextPage,
			}
			for _, cm := range n.Comments.Nodes {
				t.CommentNodeIDs = append(t.CommentNodeIDs, cm.ID)
			}
			if len(n.Comments.Nodes) == 0 {
				// A thread with no comments cannot be attributed, so it is nobody's to close.
				out = append(out, t)
				continue
			}
			first := n.Comments.Nodes[0]
			t.Marker = markerIn(first.Body)
			t.Author = first.Author.Login

			seen := make(map[string]struct{}, len(n.Comments.Nodes))
			for _, cm := range n.Comments.Nodes {
				seen[cm.Author.Login] = struct{}{}
			}
			t.Participants = len(seen)
			out = append(out, t)
		}

		if !threads.PageInfo.HasNextPage || threads.PageInfo.EndCursor == "" {
			return out, nil
		}
		cursor := threads.PageInfo.EndCursor
		after = &cursor
	}
	// The cap was reached with pages still to come. Returning what was read would be
	// reported as a complete pass over the conversations, and every thread past the cap
	// would stay open forever while the reviewer said it had tidied up. A partial read
	// is not a read.
	return nil, fmt.Errorf("github: more than %d pages of review threads", maxThreadPages)
}

// resolveThreadMutation closes one conversation.
const resolveThreadMutation = `mutation($threadId:ID!){
  resolveReviewThread(input:{threadId:$threadId}){ thread{ id isResolved } }
}`

// resolveThread marks a review thread resolved.
func (c *client) resolveThread(ctx context.Context, threadID string) error {
	return c.graphql(ctx, resolveThreadMutation, map[string]any{"threadId": threadID}, nil)
}

// minimizeCommentMutation collapses a comment as outdated.
const minimizeCommentMutation = `mutation($subjectId:ID!){
  minimizeComment(input:{subjectId:$subjectId, classifier:OUTDATED}){ minimizedComment{ isMinimized } }
}`

// minimizeComment collapses one of the reviewer's own comments, marking it outdated.
//
// It is the retraction available to a reviewer that may not resolve. Resolving a
// conversation needs write access to the repository; a reviewer holding only
// pull_requests:write can still minimize its own comment, which folds it away and
// labels it outdated without deleting what it said. The conversation stays on the
// record, and stays unresolved.
func (c *client) minimizeComment(ctx context.Context, commentNodeID string) error {
	return c.graphql(ctx, minimizeCommentMutation, map[string]any{"subjectId": commentNodeID}, nil)
}
