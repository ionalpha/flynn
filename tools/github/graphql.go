package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// graphqlURL derives the GraphQL endpoint, used only because REST cannot
// resolve threads. GitHub Enterprise serves REST at /api/v3 and GraphQL at
// /api/graphql, so the version segment is replaced; any other base, including
// github.com and test servers, gets /graphql appended.
func graphqlURL(apiBase string) string {
	base := strings.TrimSuffix(apiBase, "/")
	if strings.HasSuffix(base, "/api/v3") {
		return strings.TrimSuffix(base, "/v3") + "/graphql"
	}
	return base + "/graphql"
}

type graphqlError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// graphql fails on a non-empty `errors` array, because GraphQL answers 200 on
// failure and a permission error would otherwise read as "no threads".
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

type ReviewThread struct {
	// ID is the GraphQL node id that resolveReviewThread takes, which differs
	// from the REST comment id.
	ID string

	Resolved bool

	// Outdated means the anchored hunk no longer matches the head.
	Outdated bool

	// Marker comes from the first comment's body; a human's thread has none.
	Marker string

	Author string

	// Participants counts distinct commenters; more than one means a reply.
	Participants int

	// CanResolve is GitHub's per-thread answer. Resolving needs repository write
	// access, so a reviewer with only pull_requests:write never can.
	CanResolve bool

	// CommentNodeIDs are the comments' GraphQL ids, in order, for minimizeComment.
	CommentNodeIDs []string

	// Truncated means unread comments remain, so a reply cannot be ruled out.
	Truncated bool
}

// reviewThreadsQuery pages over threads, so threads past the first page still
// get resolved. The 100-comment limit is per thread.
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

// maxThreadPages bounds pagination so a bad response cannot loop forever.
const maxThreadPages = 20

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
	// Fail at the cap, because a partial list would pass as complete and leave
	// later threads open.
	return nil, fmt.Errorf("github: more than %d pages of review threads", maxThreadPages)
}

// viewerQuery works for both credential kinds; REST's /user returns 403 to an
// installation token. An App answers with its suffixed login ("vouchbot[bot]").
const viewerQuery = `{ viewer { login } }`

func (c *client) viewerLogin(ctx context.Context) (string, error) {
	var resp struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := c.graphql(ctx, viewerQuery, nil, &resp); err != nil {
		return "", err
	}
	if resp.Viewer.Login == "" {
		return "", errors.New("github: the credential names no viewer")
	}
	return resp.Viewer.Login, nil
}

const resolveThreadMutation = `mutation($threadId:ID!){
  resolveReviewThread(input:{threadId:$threadId}){ thread{ id isResolved } }
}`

func (c *client) resolveThread(ctx context.Context, threadID string) error {
	return c.graphql(ctx, resolveThreadMutation, map[string]any{"threadId": threadID}, nil)
}

const minimizeCommentMutation = `mutation($subjectId:ID!){
  minimizeComment(input:{subjectId:$subjectId, classifier:OUTDATED}){ minimizedComment{ isMinimized } }
}`

// minimizeComment folds a comment away as outdated without deleting it. It
// needs only pull_requests:write, so it is the retraction left when resolving
// is not allowed.
func (c *client) minimizeComment(ctx context.Context, commentNodeID string) error {
	return c.graphql(ctx, minimizeCommentMutation, map[string]any{"subjectId": commentNodeID}, nil)
}
