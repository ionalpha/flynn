# Exercise sets

One directory per skill, named for the skill:

    evals/<skill-name>/
      exercises.txt   required, written with the skill
      holdout.txt     optional to load, needed in practice, written by someone else
      fixtures/       optional, one directory per starting state

Each line is one exercise: an objective in the words a user would give it, then the
command that decides whether the run did it.

    the tests fail after my change and I cannot see why | go test ./...
    add a column to the users table without downtime    | ./verify-migration.sh

`#` opens a comment and blank lines are ignored. Both columns are required. An
exercise with no verifier has no outcome, and admitting one would put a run in the
tally that nothing graded.

## Fixtures: the state an exercise starts from

A trial begins in an empty directory. That suits "write me a parser" and is useless
for "the tests fail after my change", which has to be given the failing tests. A row
may open with a fixture in brackets, naming a directory under `fixtures/` that is
copied into the working directory before the run starts:

    [broken-parser] the tests fail after my change | go test ./... 2>&1 | grep -q ok

Both arms get the same fixture, copied fresh, so nothing one trial does to it reaches
the next. The fixture goes in front rather than in a third column so the verifier
stays the last field and can hold as many pipes as a shell command needs. A fixture
is one directory: no separators in the name, no symbolic links inside it, and an
empty one is refused because it seeds nothing while looking like it seeded something.

A named fixture that is not there fails when the set loads, before the first model
call.

## Verifiers run on three operating systems

A verifier is handed to the sandbox as one command line, and the sandbox gives it to
whichever shell the host guarantees: `sh -c` on macOS and Linux, `cmd.exe /s /c` on
Windows. So the line has to be one both shells read the same way. A loop, a `$VAR`, a
`[ -f x ]`, or a single-quoted argument is a bashism, and on Windows it either fails to
parse or silently means something else, which grades a run that was never run.

Put the logic in an interpreter instead, and keep the shell to `&&` and the exit
status. `python3 -c "..."` with double quotes outside and single quotes inside reads
identically on all three:

    python3 -c "import sys;sys.exit(0 if open('out.txt').read().strip()=='ok' else 1)"

Anything the verifier invokes has to be on `PATH` on all three, which python3, node, and
go are and a shell builtin from one family is not. The measurement is only comparable
across hosts if the same line means the same thing on each.

## Running it

    flynn skill ab <skill-name> --repeats 3

Every exercise runs that many times with the skill and the same number of times
without it, in a fresh store and a fresh working directory each time, and the
verifier grades each run after the agent has stopped. The report gives one verdict:
helped, no measurable difference, or hurt.

## Writing a set that can measure something

Five to ten exercises, each a real job with a command that passes or fails on its
own. The verifier decides everything, so it has to be a check the run either
satisfies or does not: a grep for a word the model was likely to write is not one.

Aim at work the model gets right sometimes. An exercise it always passes and one it
never passes both say nothing about the skill, because the harness reads only the
runs where the two conditions disagreed. It reports that outright when there were
none. If every skill passes the first time you measure it, the exercises are too
easy rather than the library being good.

## Why the holdout exists

A skill written against its own exercises passes its own exercises. `holdout.txt` is
the half someone other than the skill's author writes, and the report scores it
separately, so a skill that helps on one half and does nothing on the other is
visible rather than averaged away.

## Why these files are not in the pack

The agent can read a skill's own directory through `skill_resource`. A set shipped
beside the skill would be readable by the run being measured, which turns the eval
into a target. Nothing here goes into the binary.
