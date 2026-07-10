package diag

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/secret"
)

// argWord generates a command-line token. The alphabet is small and deliberately
// adversarial: it mixes the characters that steer the redactor (a leading dash, an
// inline '=') with the literal substrings it keys on, so rapid reaches the
// interesting branches instead of wandering through arbitrary Unicode.
func argWord() *rapid.Generator[string] {
	return rapid.SampledFrom([]string{
		"flynn", "goal", "auth", "runs", "get", "serve", "set",
		"-v", "--verbose", "--model", "anthropic:opus", "--data-dir", "/tmp/d",
		"--token", "--api-key", "--API-KEY=inline", "--no-auth", "--profile", "/tmp/b",
		"-", "--", "=", "a=b", "", "  ", "[REDACTED]",
		"sk-live-abcdefghijklmnop", "hf_abcdefghijklmnop", "AKIAabcdefghijklmnop",
		"delete everything", "sk-1",
	})
}

// argv generates a whole command line: a program path followed by tokens.
func argv() *rapid.Generator[[]string] {
	return rapid.Custom(func(t *rapid.T) []string {
		rest := rapid.SliceOfN(argWord(), 0, 8).Draw(t, "rest")
		return append([]string{"flynn"}, rest...)
	})
}

// The redactor's shape is a contract the manifest depends on: a reader lines the
// recorded argv up against what it knows of the command, so a redaction that drops
// or adds a token would silently misattribute every argument after it.
func TestPropertyRedactArgsPreservesShapeAndProgramPath(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		args := argv().Draw(t, "args")

		got := RedactArgs(args)

		if len(got) != len(args) {
			t.Fatalf("RedactArgs(%q) returned %d tokens, want %d", args, len(got), len(args))
		}
		if got[0] != args[0] {
			t.Fatalf("RedactArgs(%q) rewrote the program path to %q", args, got[0])
		}
	})
}

// Everything after a free-text subcommand is user text, whatever it looks like. A
// token that happens to resemble a flag is not thereby safe to record: an objective
// can contain anything, including "--token".
//
// The subcommand is identified by the one thing observable from outside: a
// free-text command that came back unredacted is a free-text command the redactor
// recognized as such. A "goal" that was swallowed as some sensitive flag's value
// comes back as secret.Redacted and starts nothing, which is why this looks at the
// output rather than re-deriving the redactor's parse from the input.
func TestPropertyRedactArgsWithholdsEverythingAfterAFreeTextSubcommand(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		args := argv().Draw(t, "args")

		got := RedactArgs(args)

		first := -1
		for i := 1; i < len(args); i++ {
			if freeTextCommands[args[i]] && got[i] == args[i] {
				first = i
				break
			}
		}
		if first < 0 {
			return // no free-text subcommand was recognized in this argv
		}

		for i := first + 1; i < len(args); i++ {
			if got[i] != secret.Redacted {
				t.Fatalf("RedactArgs(%q): token %d (%q) follows the free-text subcommand %q at %d but survived as %q",
					args, i, args[i], args[first], first, got[i])
			}
		}
	})
}

// A bare credential is withheld wherever it appears. No branch of the redactor may
// let one through, whatever precedes it.
func TestPropertyRedactArgsWithholdsEveryRecognizableCredential(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		args := argv().Draw(t, "args")

		got := RedactArgs(args)

		for i := 1; i < len(args); i++ {
			if looksLikeCredential(args[i]) && got[i] != secret.Redacted {
				t.Fatalf("RedactArgs(%q): credential-shaped token %d (%q) survived as %q", args, i, args[i], got[i])
			}
		}
	})
}

// Redaction is a fixed point. A manifest that is read, re-recorded, and written
// again must not decay: a second pass may withhold no more and no less than the
// first, or an argv would erode token by token across a bundle's life.
func TestPropertyRedactArgsIsIdempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		args := argv().Draw(t, "args")

		once := RedactArgs(args)
		twice := RedactArgs(once)

		if !slices.Equal(once, twice) {
			t.Fatalf("RedactArgs is not idempotent on %q:\n once %q\ntwice %q", args, once, twice)
		}
	})
}

// RedactArgs must never alias or mutate the caller's argv: the manifest outlives
// the os.Args slice it was built from.
func TestPropertyRedactArgsNeverMutatesItsInput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		args := argv().Draw(t, "args")
		before := slices.Clone(args)

		RedactArgs(args)

		if !slices.Equal(args, before) {
			t.Fatalf("RedactArgs mutated its input: %q became %q", before, args)
		}
	})
}

// The manifest's whole purpose is that a bundle copied off a machine can be shown
// to be intact. Whatever the bundle holds, every member is hashed, sized, sorted,
// and the manifest never hashes itself.
func TestPropertyHashMembersCoversEveryFileExactlyOnce(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		contents := rapid.SliceOfN(rapid.SliceOfN(rapid.Byte(), 0, 64), 0, 6).Draw(t, "members")
		withManifest := rapid.Bool().Draw(t, "withManifest")

		// rapid.T is not a *testing.T, so it has no TempDir; the directory is made and
		// removed by hand for each property run.
		dir, err := os.MkdirTemp("", "diag-property")
		if err != nil {
			t.Fatalf("temp dir: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()

		want := make(map[string][]byte, len(contents))
		for i, data := range contents {
			name := fmt.Sprintf("member-%d.pprof", i)
			writeMember(t, dir, name, data)
			want[name] = data
		}
		if withManifest {
			// A rewritten bundle already holds a manifest; it must never hash itself.
			writeMember(t, dir, MemberManifest, []byte("{}"))
		}

		members, err := hashMembers(dir, MemberManifest)
		if err != nil {
			t.Fatalf("hashMembers: %v", err)
		}

		if len(members) != len(want) {
			t.Fatalf("hashMembers returned %d members, want %d (the manifest is never a member)", len(members), len(want))
		}
		if !sort.SliceIsSorted(members, func(i, j int) bool { return members[i].Name < members[j].Name }) {
			t.Fatalf("hashMembers returned unsorted members: %v", members)
		}

		seen := make(map[string]bool, len(members))
		for _, m := range members {
			if m.Name == MemberManifest {
				t.Fatalf("hashMembers hashed the manifest itself")
			}
			if seen[m.Name] {
				t.Fatalf("hashMembers listed %q twice", m.Name)
			}
			seen[m.Name] = true

			data, ok := want[m.Name]
			if !ok {
				t.Fatalf("hashMembers invented a member %q", m.Name)
			}
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != m.SHA256 {
				t.Fatalf("member %q: hash %s, want %s", m.Name, m.SHA256, got)
			}
			if int64(len(data)) != m.Bytes {
				t.Fatalf("member %q: size %d, want %d", m.Name, m.Bytes, len(data))
			}
		}
	})
}

// writeMember writes one bundle member during a property run.
func writeMember(t *rapid.T, dir, name string, data []byte) {
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
