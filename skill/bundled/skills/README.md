# The bundled skill pack

Every directory here is one skill, shipped inside the binary and seeded into the
reserved `@bundled` scope on start. Adding a skill to the product means adding a
directory here; there is no registration step and no list to keep in sync.

A skill directory follows the Agent Skills layout, which `skill/skillmd` implements
and the conformance test in this package enforces on every directory below:

    <skill-name>/
      SKILL.md        required
      scripts/        optional
      references/     optional
      assets/         optional

`SKILL.md` is YAML frontmatter then Markdown. `name` must equal the directory name
and `description` is required: it is the only text a runtime reads at discovery, so
a skill without one is invisible until something has already decided to read it.

    ---
    name: skill-name
    description: What the skill is for, and when to reach for it.
    ---

    The body.

Our own fields ride in `metadata` under the `ionagent.io/` prefix (`title`, `tags`,
`check`); see `skill/skillmd/metadata.go` for the convention and `skill/pack.go` for
what crosses into a stored skill and what does not.

## What the prose gate refuses

`TestPackProseIsAuthored` reads every file below this one, including reference
documents and scripts, and fails on marks a shipped skill must not carry:

- The em-dash, the horizontal bar, the en-dash, and the single-character ellipsis.
  Write a colon, a semicolon, parentheses, a full stop, a hyphen, the word "to", or
  three dots.
- An identifier from the system this work is planned in: a `@task:` or `@note:` link,
  a bare record id next to the words task, note or epic, or a UUID. A reader outside
  this workspace cannot follow one, so say the thing rather than pointing at where it
  is recorded.
- The names of the workspace this was authored in, and of the other skill libraries.
  A skill teaches its craft; comparing the shelf it sits on to another shelf dates
  immediately.

Every refusal is reported at once, with its file, line, column and the text, so a
pack is fixed in one pass rather than one CI run per mark.

There is no vocabulary check and no escape hatch, deliberately. Every rule above
catches something with no legitimate use in a skill, which is why it can refuse
without an appeal; a check on ordinary words needs an escape designed first, or it
gets switched off the first week it is wrong.

## What retrieval.txt is for

`retrieval.txt` beside this file is the pack's retrieval table: one row per
objective, naming the skills that objective must be offered and the skills it must
not be. `TestPackIsRetrievable` runs the runtime's own ranker over the real pack
against every row, with no model and no tokens.

It exists because a description is the only text loaded at discovery, so getting one
wrong does not fail anything. The skill is simply never offered, the run proceeds
without it, and the only other signal is a counter someone has to think to read.

Adding a skill means adding a row. `TestEveryPackSkillStatesItsTriggers` refuses a
skill that no row expects to be offered, because the objectives a skill is reached
for are part of its design and are worth writing before its body: a skill whose
trigger set cannot be stated does not yet have a scope.

Neither file is a skill. `LoadAll` reads directories and ignores the rest.
