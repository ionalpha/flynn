package github

import "testing"

const (
	ourMarker   = markerPrefix + "abc123 -->"
	otherMarker = markerPrefix + "def456 -->"
)

// self is the reviewer's own login in these tests.
const self = "vouchbot[bot]"

// ours builds a thread the reviewer opened, with no reply.
func ours(marker string) ReviewThread {
	return ReviewThread{ID: "T1", Marker: marker, Author: self, Participants: 1}
}

// TestResolvableThread is the whole of the reviewer's authority to close a
// conversation, stated as a table. Every "false" row is a conversation that stays
// open, and each one exists because closing it would destroy something: a person's
// reply, a live defect, or a finding the reviewer itself just raised again.
func TestResolvableThread(t *testing.T) {
	raisedAgain := map[string]bool{ourMarker: true}
	nothingFound := map[string]bool{}

	cases := []struct {
		name   string
		thread ReviewThread
		found  map[string]bool
		want   bool
		reason string
	}{
		{
			name:   "ours, gone, outdated",
			thread: func() ReviewThread { t := ours(ourMarker); t.Outdated = true; return t }(),
			found:  nothingFound,
			want:   true,
		},
		{
			name:   "ours, gone, not outdated",
			thread: ours(ourMarker),
			found:  nothingFound,
			want:   true,
		},
		{
			name:   "ours, but raised again this review",
			thread: ours(ourMarker),
			found:  raisedAgain,
			want:   false,
			reason: "the finding was raised again in this review",
		},
		{
			name:   "ours, raised again, and outdated anyway",
			thread: func() ReviewThread { t := ours(ourMarker); t.Outdated = true; return t }(),
			found:  raisedAgain,
			want:   false,
			reason: "the finding was raised again in this review",
		},
		{
			name:   "a human opened it",
			thread: ReviewThread{ID: "T2", Marker: "", Author: "a-person", Participants: 1, Outdated: true},
			found:  nothingFound,
			want:   false,
			reason: "opened by someone else",
		},
		{
			// The marker is plain text in a comment body. A person who quotes a finding
			// back at the reviewer has quoted the key to their own conversation, and the
			// reviewer must not take that as licence to close it. The author is the fact.
			name:   "a human's thread carrying a copied marker",
			thread: ReviewThread{ID: "T3", Marker: ourMarker, Author: "a-person", Participants: 1, Outdated: true},
			found:  nothingFound,
			want:   false,
			reason: "opened by someone else",
		},
		{
			name:   "our own comment, but not a finding",
			thread: ReviewThread{ID: "T4", Marker: "", Author: self, Participants: 1, Outdated: true},
			found:  nothingFound,
			want:   false,
			reason: "not a finding",
		},
		{
			name:   "ours, but someone replied",
			thread: func() ReviewThread { t := ours(ourMarker); t.Participants = 2; t.Outdated = true; return t }(),
			found:  nothingFound,
			want:   false,
			reason: "someone replied",
		},
		{
			name:   "already resolved",
			thread: func() ReviewThread { t := ours(ourMarker); t.Resolved = true; return t }(),
			found:  nothingFound,
			want:   false,
			reason: "already resolved",
		},
		{
			name:   "ours, a different finding is still open",
			thread: ours(ourMarker),
			found:  map[string]bool{otherMarker: true},
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := resolvableThread(tc.thread, self, tc.found)
			if got != tc.want {
				t.Fatalf("resolvableThread = %v (%s), want %v", got, reason, tc.want)
			}
			if !tc.want && reason != tc.reason {
				t.Errorf("refused for %q, want %q", reason, tc.reason)
			}
		})
	}
}

// A reviewer with no configured identity cannot tell its own conversation from anyone
// else's, so it closes none rather than guessing from a marker anybody can copy.
func TestNothingResolvesWithoutAnIdentity(t *testing.T) {
	ok, reason := resolvableThread(ours(ourMarker), "", map[string]bool{})
	if ok {
		t.Fatal("a reviewer with no identity must not resolve anything")
	}
	if reason != "the reviewer has no identity to check the author against" {
		t.Errorf("refused for %q", reason)
	}
}

// TestGraphQLURL: GitHub Enterprise serves REST under /api/v3 and GraphQL under
// /api/graphql, not /api/v3/graphql. Appending the segment would point every
// enterprise install at a 404, and the reviewer would silently resolve nothing.
func TestGraphQLURL(t *testing.T) {
	cases := map[string]string{
		"https://api.github.com":              "https://api.github.com/graphql",
		"https://api.github.com/":             "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3":      "https://ghe.example.com/api/graphql",
		"https://ghe.example.com/api/v3/":     "https://ghe.example.com/api/graphql",
		"http://127.0.0.1:8080":               "http://127.0.0.1:8080/graphql",
		"https://ghe.example.com/base/api/v3": "https://ghe.example.com/base/api/graphql",
	}
	for base, want := range cases {
		if got := graphqlURL(base); got != want {
			t.Errorf("graphqlURL(%q) = %q, want %q", base, got, want)
		}
	}
}
