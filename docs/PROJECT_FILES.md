# Project instruction files

A repository can carry files that tell an agent how to work in it: `AGENTS.md`,
`CLAUDE.md`, and `AGENTS.override.md`. Flynn can add them to the model's instructions for
a session or a run. Anyone who can change the repository can change these files, so
Flynn loads them only after a visible decision, treats what they say as untrusted input,
and records the decision on the run.

## Which files load

Flynn finds the root of the checkout (the nearest directory above the working directory
with a `.git` entry; with none, only the working directory counts) and reads the three
names in every directory from the root down to the working directory. The result is
ordered outermost first, so the file closest to your work is read last and wins when two
disagree.

- `AGENTS.override.md` in a directory replaces `AGENTS.md` in that directory. The
  replaced file is reported as not loaded.
- A file whose content is identical to one already loaded in the same directory is
  dropped, so a repository that keeps `CLAUDE.md` as a copy of `AGENTS.md` adds the text
  once.
- A file that is a symlink pointing outside the repository, is not a regular file, is
  empty, or looks binary is not loaded.
- Each file is cut at 32 KiB and the set at 96 KiB. When the set is over budget, files
  nearest the working directory are kept.
- Control characters and invisible Unicode formatting characters are removed from the
  text. What the memory guard's screen flags (hidden-instruction smuggling or overt
  injection phrasing) is shown when you are asked and recorded with the file; flagged
  phrasing is reported, not deleted.

Each loaded file is added to the model's instructions inside a fence that names the path
and the file's SHA-256. The closing tag carries the same hash, and a file cannot close
its own fence early.

## The trust decision

The decision is made once, before the first turn, and applies to every run the session
opens. It follows this order:

1. `--project-files=trust` or `--project-files=skip` (or `FLYNN_PROJECT_FILES`, which the
   flag overrides) decides outright.
2. Otherwise, if you chose "always" for this exact content in this checkout before, the
   files load.
3. Otherwise, at a terminal, Flynn lists the files and asks: `[y]` this session, `[a]`
   always for this content, `[N]` no. Only an explicit yes loads them.
4. Otherwise (no terminal, such as a script or CI job) the files are not loaded and the
   run says so, with the flag that would load them.

"Always" is remembered in `project-trust.json` in the data directory, keyed by the
checkout and a digest of the files' content. Editing a file, or adding one, asks again.

`flynn goal` and the interactive session make this decision. An external agent CLI reads
its own project files, so a session driven through one does not.

## Taint

Loaded files enter the run as untrusted context. The session's taint scope is marked with
one `external:project-file:<path>` reason per file, so a memory distilled from a session
that read them is stored as tainted and is not pushed into later sessions by the wake
digest. This is the same mechanism that taints a session after a shell command or a web
fetch.

## The record

The decision is written to the run's stream as a `project.context` event: whether the
files loaded, on what basis (`flag`, `remembered`, `prompt` or `unattended`), the checkout
root, a digest of the set, and for each file its path, SHA-256 and size, with any
truncation or findings. The same line appears in the transcript, in `flynn inspect` and
in the full-screen interface:

```
project files loaded as untrusted context (prompt): AGENTS.md, pkg/CLAUDE.md
project files not loaded (unattended): AGENTS.md; --project-files=trust loads them
```

A session continued with `/clear` or `/compact` starts a new run and records the decision
again on it.

## Limits

`flynn resume` does not load project files into the run it continues.
