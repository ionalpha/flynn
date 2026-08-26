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

Our own fields ride in `metadata` under the `flynnhq.com/` prefix (`title`, `tags`,
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

Every skill directory carries a `retrieval.txt`: one row per objective, in the words
someone would actually type, naming the skills that objective must be offered and the
skills it must not be.

    objective | must be offered | must not be offered

Both skill columns are comma-separated and either may be empty, `#` opens a comment,
and blank lines are ignored. A row must name its own directory's skill in one of the
two columns, which is what keeps the file the whole of what the pack claims about that
skill. `TestPackIsRetrievable` runs the runtime's own ranker over the real pack against
every row of every file, with no model and no tokens, so a row is checked on each build.

The file exists because a description is the only text loaded at discovery, so getting
one wrong does not fail anything. The skill is simply never offered, the run proceeds
without it, and the only other signal is a counter someone has to think to read weeks
later.

The second skill column catches the harder fault. A description that misses its own
subject hides one skill. A description that reaches into another skill's subject
outranks the better match on every objective it takes, which degrades the pack around
it and is not something the author of that skill would think to look for. Writing a
negative row means reading the neighbouring skill's claims, so the two files are worth
opening together.

Adding a skill means adding its rows. `TestEveryPackSkillStatesItsTriggers` refuses a
skill that no row expects to be offered, because the objectives a skill is reached
for are part of its design and are worth writing before its body: a skill whose
trigger set cannot be stated does not yet have a scope.

The rows sit inside each skill rather than in one table beside the pack so that adding
a skill is an added file. A single table gives every branch the same last line to
append to, so two skills authored in the same week conflict on text no reviewer reads.

Neither this file nor `retrieval.txt` is a skill. `LoadAll` reads directories and
ignores the rest.
