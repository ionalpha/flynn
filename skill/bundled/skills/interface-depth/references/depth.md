# Three surfaces, counted

Worked examples of the counting in SKILL.md. Each one starts at a call site, lists
the facts a caller holds, and says which of the four removals applied. The code is
from the repository this pack ships in, so the counts can be checked rather than
taken.

## One: the error a caller used to have to interpret

**The call site.** Something failed. The caller has an `error` and has to decide
whether to try again, stop, ask a person, or record the run as over budget.

**Facts the caller held before the surface existed.** Which subsystem produced the
failure, so it knew which error values to compare against. Which of those values mean
a network blip and which mean bad input. What a transport error looks like when it
arrives through a queue rather than a function return. What to assume about an error
it has never seen. That is four facts, at least, and they were held differently in
each place, which is the actual damage: retry policy was being decided by whoever
wrote each call site.

**The surface.** The `fault` package is 111 lines and exports six names. The one that
carries the weight is `Classify(err error) Class`, which answers a single question,
being how the caller should react, and returns one of six values. `CodeOf(err error)`
answers a deliberately different question, which is which rule spoke, because a
recorded refusal needs both and neither substitutes for the other.

**The removal.** Two of the four kinds. Error work moved inside: unknown failures are
Terminal, so the caller never decides what to assume, and opting into a retry means
classifying explicitly at the place that knows. Internals went too: no call site
matches on an error string or names the package that produced the failure.

**The count.** 28 non-test call sites across 20 packages, 314 including tests. Every
one of them switches on a value from a closed set instead of interpreting an error,
and none of them can be wrong about a subsystem it does not import.

**Why this is depth and not a helper.** The sentence finishes: a caller no longer has
to know which failures are worth retrying. A helper that turned an error into a
string would not have finished it.

## Two: the filesystem as a parameter

**The call site.** Load a pack of skills. Two callers exist, and they are as different
as they get: one reads a directory a user is editing, the other reads the pack
compiled into the executable.

**The fact that was nearly baked in.** Where the bytes come from. Written the obvious
way, the loader takes a path, the embedded pack becomes a special case, and every
rule of the format gets a second implementation for the in-binary copy. The rules
would then disagree, quietly, and the copy that ships to every install is the one
nobody runs the conformance test against.

**The surface.** `skillmd.Load(fsys fs.FS, dir string)` takes the filesystem as an
argument, and `bundled.FS()` hands it the embedded one. The package documentation says
what that buys in one line: it is the format, from a filesystem that happens to live
in the executable.

**The removal.** A precondition and an internal, taken away by the general shape
rather than by an option. Note what did not happen. Nobody added a `fromEmbed bool`,
and nobody added a second entry point. The parameter that was already general enough
to cover both cases replaced the branch, which is the move at the end of the argument
section in SKILL.md.

**The check that keeps it honest.** Because the loader cannot tell where its bytes
came from, the test that reads a pack from a temporary directory and the test that
reads the shipped pack exercise the same code. A special case would have split them.

## Three: the surface where the right answer was to leave it

**The call site.** The pack's prose gate. A test asks whether any file in the shipped
pack carries a mark an authored skill must not carry, and fails with the list.

**What a redesign would have been tempted by.** `skillstyle` exports both
`Check(path, content)` for one file and `CheckFS(fsys, root)` for a tree, plus
`Report(findings)` for the rendering. Three exported functions where two look like
they could be one, and `CheckFS` does call `Check` underneath. It has the shape people
collapse on sight.

**Why the sentence finishes for each of them.** `CheckFS` removes the walk, the
ordering, and one piece of error work: a file it cannot read becomes a finding rather
than an error return, so a single broken path cannot end the walk and report a clean
pack. That is a fact the caller would otherwise have had to get right, and most
callers would have got it wrong in the direction of silence. `Check` stays exported
because a caller that already has the bytes should not have to invent a filesystem to
be checked. `Report` returns the empty string for no findings, so no caller branches
on a count before rendering: the case does not arise.

**The reading that settled it.** Counting facts at the two call sites gave zero
repeated steps. Nothing was being carried in either place, so there was nothing to
remove, and the only available change was to move code around behind a name. The
measured position in SKILL.md applies directly: no success-rate gain is on offer here,
and a leaf that reads cleanly at both of its call sites is finished.

**What to say in that situation.** Report that you looked and found nothing to take
away. A depth review that returns nothing is a result, and it is a great deal more
useful than one that manufactures a layer to justify the time.

## The check, per ecosystem

The claim is that the behaviour is reachable from outside the module. Make the
compiler or the import system prove it.

| Ecosystem | The form it takes |
|---|---|
| Go | A test file declaring `package thing_test` can reach only what `thing` exports. |
| Python | Import the package, not the module inside it, and keep the test to the names in `__all__`. |
| TypeScript | Import from the package entry point, never a deep relative path into its source. |
| Java, Kotlin | Put the test class in a different package and drop the visibility relaxations. |
| Rust | Put the test in `tests/`, which links the crate as an external consumer does. |

In every case the failure mode you are looking for is the same. If making the test
compile requires reaching for a name that a real caller could not reach, the behaviour
is not available at the surface, and the name you had to reach for is the specification
of what is missing.
