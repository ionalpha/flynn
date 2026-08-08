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

This file is not a skill. `LoadAll` reads directories and ignores the rest.
