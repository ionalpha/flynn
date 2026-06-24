package learn

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/state"
)

// TestSandboxVerifier runs real checks through a local sandbox: a command that
// exits 0 verifies, a non-zero exit ran-but-failed, and a skill with no check (or
// a memory item) is unproven rather than broken.
func TestSandboxVerifier(t *testing.T) {
	v := NewSandboxVerifier(func(context.Context) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(t.TempDir())
	})
	cases := []struct {
		name              string
		lesson            Lesson
		wantRan, verified bool
	}{
		{"passing check", Lesson{Kind: LessonSkill, Check: "exit 0"}, true, true},
		{"failing check", Lesson{Kind: LessonSkill, Check: "exit 3"}, true, false},
		{"no check", Lesson{Kind: LessonSkill}, false, false},
		{"blank check", Lesson{Kind: LessonSkill, Check: "   "}, false, false},
		{"memory ignored", Lesson{Kind: LessonMemory, Check: "exit 0"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v.Verify(context.Background(), tc.lesson, state.Scope{})
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if got.Ran != tc.wantRan || got.Verified != tc.verified {
				t.Fatalf("verdict = %+v, want Ran=%v Verified=%v", got, tc.wantRan, tc.verified)
			}
		})
	}
}

// TestSandboxVerifierCancelled confirms a cancelled context is a hard error, not a
// silent "unproven" verdict, so a torn-down run does not quietly keep skills.
func TestSandboxVerifierCancelled(t *testing.T) {
	v := NewSandboxVerifier(func(context.Context) (sandbox.Sandbox, error) {
		return sandbox.NewLocal(t.TempDir())
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Verify(ctx, Lesson{Kind: LessonSkill, Check: "exit 0"}, state.Scope{}); err == nil {
		t.Fatal("a cancelled context must surface as an error")
	}
}

// fakeVerifier returns a fixed verdict, so the curator's policy is tested without a
// sandbox.
type fakeVerifier struct {
	v   Verdict
	err error
}

func (f fakeVerifier) Verify(context.Context, Lesson, state.Scope) (Verdict, error) {
	return f.v, f.err
}

// TestCuratorVerificationPolicy checks the three-way gate: a verified skill is kept
// and tagged verified, a proven-broken one is dropped, and an unproven one is kept
// tagged unverified. Without a verifier a skill is kept untouched by verification.
func TestCuratorVerificationPolicy(t *testing.T) {
	cases := []struct {
		name        string
		verifier    Verifier
		wantStored  bool
		wantDropped bool
		wantTag     string
		noTag       string
	}{
		{"verified", fakeVerifier{v: Verdict{Verified: true, Ran: true}}, true, false, verifiedTag, unverifiedTag},
		{"broken dropped", fakeVerifier{v: Verdict{Ran: true}}, false, true, "", ""},
		{"unproven kept", fakeVerifier{v: Verdict{}}, true, false, unverifiedTag, verifiedTag},
		{"no verifier", nil, true, false, provenanceTag, verifiedTag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skills, memories := newStores(t)
			d := &fakeDistiller{lessons: []Lesson{{Kind: LessonSkill, Title: "Build", Body: "make", Check: "make"}}}
			var opts []Option
			if tc.verifier != nil {
				opts = append(opts, WithVerifier(tc.verifier))
			}
			captured, err := NewCurator(d, skills, memories, opts...).Curate(context.Background(), convergedOutcome())
			if err != nil {
				t.Fatal(err)
			}

			if got := len(captured.Skills) == 1; got != tc.wantStored {
				t.Fatalf("stored=%v, want %v (captured %+v)", got, tc.wantStored, captured)
			}
			if got := len(captured.Dropped) == 1; got != tc.wantDropped {
				t.Fatalf("dropped=%v, want %v", got, tc.wantDropped)
			}
			if !tc.wantStored {
				return
			}
			sk := captured.Skills[0]
			if tc.wantTag != "" && !hasTag(sk.Tags, tc.wantTag) {
				t.Fatalf("skill tags %v missing %q", sk.Tags, tc.wantTag)
			}
			if tc.noTag != "" && hasTag(sk.Tags, tc.noTag) {
				t.Fatalf("skill tags %v should not contain %q", sk.Tags, tc.noTag)
			}
		})
	}
}

// TestCuratorVerifierErrorAborts confirms a verifier error (not a failed check)
// aborts the run rather than silently dropping or keeping skills.
func TestCuratorVerifierErrorAborts(t *testing.T) {
	skills, memories := newStores(t)
	d := &fakeDistiller{lessons: []Lesson{{Kind: LessonSkill, Title: "x", Body: "y", Check: "z"}}}
	boom := fakeVerifier{err: context.Canceled}
	if _, err := NewCurator(d, skills, memories, WithVerifier(boom)).Curate(context.Background(), convergedOutcome()); err == nil {
		t.Fatal("a verifier error must abort Curate")
	}
}
