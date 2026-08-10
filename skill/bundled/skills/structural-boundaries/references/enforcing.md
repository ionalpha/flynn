# Turning a structural rule into something that runs

A boundary you cannot fail a build on is a description of last year's intention. This
is the mechanical half: how to write the rule, where it lives per ecosystem, how to
introduce one into code that already breaks it, and which rules are not worth having.

## The form every one of these takes

Three parts, and if you cannot fill in all three you do not yet have a rule:

1. **A set of files or packages** the rule is about, named by pattern.
2. **What they may not import**, named the same way.
3. **A message** that says which crossing happened and what to do instead.

Everything below is those three parts in different syntax. Write the rule as a denial
rather than a permission where the tool offers both: a deny list fails when somebody
adds a new violating import, and an allow list silently permits every package nobody
thought to list.

## Where it goes, per ecosystem

| Ecosystem | Where the rule lives |
|---|---|
| Go | `depguard` in the `golangci-lint` configuration, which fails on a denied import path with your own message. For anything it cannot say, a test using `go/packages` to walk the import graph and assert on it. |
| Python | `import-linter`, whose contracts express layers, independence between packages, and forbidden imports, run as a step in the pipeline. |
| JavaScript and TypeScript | `dependency-cruiser` for graph rules across the repository, or `no-restricted-imports` in the lint configuration for the narrow case of one package that must not be reached. |
| Java and Kotlin | `ArchUnit`, which is a normal test, so the rule runs wherever tests run and fails with a stack trace pointing at the offending class. |
| C# | `NetArchTest`, the same idea in the same place. |
| Rust | Crate boundaries plus visibility already are the rule; the compiler enforces it. What is left is dependency provenance, which is `cargo-deny`. |

Two properties decide whether one of these is worth adding, and both are about the
failure rather than the rule. It has to run in the same command the pipeline already
runs, because a check with its own invocation gets skipped. And it has to name the
file and the import in the message, because a failure that says only that a contract
was broken sends the next author to read the configuration.

## Where the ecosystem has no tool

Write a test. Walk the source tree, parse the imports with whatever the language
gives you, and assert the set. That is thirty lines in most languages and it lives
with the tests, which is where a rule survives.

A search across the tree in the pipeline is the last resort. It fails on the string
appearing in a comment and passes on the alias that avoids the string, so treat it as
a smoke alarm rather than a boundary, and say in the message that it is one.

## Adding a rule to code that already breaks it

Almost every rule worth having is already violated somewhere. Turning it on with the
violations present means either a red build nobody can fix today, or the rule not
going on at all.

Baseline instead. Record the existing violations as an explicit exception list, with
the rule live for everything else, and add one property: the list may shrink and may
never grow. New code meets the rule from the first day, the old crossings are visible
and counted rather than forgotten, and the number going down is a fact anyone can
check. Most of these tools have a mechanism for this; where one does not, the
exception list in the configuration file plus a test asserting its length does the
same job.

Put the date and the count at the top of the exception list. A list with neither
becomes permanent within a quarter.

## The register a port needs

A rule about imports says which way a dependency points. It says nothing about
whether the thing behind an interface exists, which is the failure that produces a
clean-looking architecture nobody can run. That needs a written register instead, one
row per port:

| Column | What it records |
|---|---|
| Port | The interface, by name and location. |
| Verdict | Shipped, justified, or staged. |
| Implementation | For shipped, the type and the line that wires it. For justified, empty. For staged, the type plus the switch that turns it on. |
| If absent | What the program does when nothing implements this. For a justified port, the answer must be that something stops and names it, never that the call succeeds as though one were there. |

The register is worth a check of its own, and it is a small one: a test that finds
every interface in the declared set and fails on one with no row. Without it the
register is accurate on the day it is written and misleading a month later, which is
worse than not having it, because people trust a table.

## Rules not worth writing

Each of these is easy to automate, fires constantly on correct code, and is switched
off within a month of being added, taking the useful rules beside it out of the
habit:

- **Ratios of interface size to implementation size.** A small thing that does one
  small job correctly scores as badly as a pass-through wrapper.
- **Lines per file and files per package.** These measure the symptom of the problem
  they are aimed at, and a limit is met by splitting a file in half.
- **Naming conventions dressed as architecture.** A class ending in `Service` is not
  evidence of anything.
- **Cyclomatic complexity as a gate.** Useful to read, unhelpful to fail on, because
  the branchy function is often the one that legitimately has many cases.

The rules that survive share a property: a violation is unambiguous, and the fix is
obvious from the message. An import that should not exist is either there or not.
