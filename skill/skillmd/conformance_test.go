package skillmd_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/skill/skillmd"
)

// The independent conformance check.
//
// Everything else in this package tests our reader against our writer, which proves
// they agree with each other and nothing about whether either agrees with the
// specification. skills-ref is the format's reference library, so running its
// validator over what Write produces is the one test here that can catch us being
// self-consistently wrong.
//
// The validator is a Node package, so it is not always present. The test skips when
// it is missing and fails when FLYNN_REQUIRE_SKILLS_REF is set, which is how CI asks
// for the check rather than quietly getting a pass. dev/skillsref is the script that
// installs it and sets the variable, and CI runs that script.

const requireEnv = "FLYNN_REQUIRE_SKILLS_REF"

// conformanceCorpus is written to disk and handed to the reference validator. It
// covers the fields Write can emit and the values most likely to be rendered wrong:
// a body that could be mistaken for frontmatter, values that force quoting, and a
// description at the specification's ceiling.
var conformanceCorpus = []skillmd.Doc{
	{
		Name:        "minimal",
		Description: "The two fields the format requires, and nothing else.",
	},
	{
		Name:          "every-field",
		Description:   "Every field the writer can emit.",
		License:       "Apache-2.0",
		Compatibility: "Requires Go 1.26",
		AllowedTools:  []string{"Read", "Bash"},
		Metadata: map[string]string{
			skillmd.MetaCheck: "go test ./...",
			skillmd.MetaTags:  skillmd.EncodeList([]string{"craft", "a tag with spaces"}),
			skillmd.MetaTitle: "Every field",
		},
		Body: "# Every field\n\nA body with a list:\n\n- one\n- two\n",
	},
	{
		Name:        "awkward-values",
		Description: "Values that force the writer to quote: a colon: here, a #hash, and a trailing space ",
		Metadata: map[string]string{
			skillmd.MetaCheck: "sh -c 'echo \"quoted\" && exit 0'",
			skillmd.MetaTitle: "  leading and trailing  ",
		},
		Body: "A body whose text contains a line that looks like a closer:\n\n---\n\nand keeps going.\n",
	},
	{
		Name:        "unicode-body",
		Description: "Runes outside ASCII, since the limits are counted in runes.",
		Body:        "Ünïcödé, emoji 🧪, and a CJK run 日本語テキスト.\n",
	},
	{
		Name:        "at-the-ceiling",
		Description: strings.Repeat("x", skillmd.MaxDescriptionLen),
		Body:        strings.Repeat("body line\n", 200),
	},
}

func TestReferenceValidatorAcceptsWhatWeWrite(t *testing.T) {
	t.Parallel()

	validator, err := exec.LookPath("skills-ref")
	if err != nil {
		if os.Getenv(requireEnv) != "" {
			t.Fatalf("%s is set but skills-ref is not on PATH: %v", requireEnv, err)
		}
		t.Skipf("skills-ref is not installed; run dev/skillsref to check conformance (%v)", err)
	}

	root := t.TempDir()
	if err := skillmd.WriteAll(root, conformanceCorpus); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	for _, doc := range conformanceCorpus {
		t.Run(doc.Name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command(validator, "validate", root+string(os.PathSeparator)+doc.Name)
			out, err := cmd.CombinedOutput()
			if err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					t.Fatalf("skills-ref rejected a skill we wrote:\n%s", out)
				}
				t.Fatalf("run skills-ref: %v", err)
			}
		})
	}
}
