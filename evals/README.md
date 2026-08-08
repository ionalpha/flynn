# Task sets

One directory per skill, named for the skill:

    evals/<skill-name>/
      tasks.txt     required, written with the skill
      holdout.txt   optional to load, required in practice, written by someone else

Each line is one task: an objective in the words a user would give it, then the
command that decides whether the run did it.

    the tests fail after my change and I cannot see why | go test ./...
    add a column to the users table without downtime    | ./verify-migration.sh

`#` opens a comment and blank lines are ignored. Both columns are required: a task
with no verifier has no outcome, and admitting one would put a run in the tally that
nothing graded.

## Running it

    flynn skill ab <skill-name> --repeats 3

Every task runs that many times with the skill and the same number of times without
it, in a fresh store and a fresh working directory each time, and the verifier grades
each run after the agent has stopped. The report gives one verdict: helped, no
measurable difference, or hurt.

## Writing a task set that can measure something

Five to ten tasks, each a real job with a command that passes or fails on its own.

The verifier decides everything, so it has to be a check the run either satisfies or
does not. A grep for a word the model was likely to write is not a verifier.

Aim at tasks the model gets right sometimes. A task it always passes and a task it
never passes both tell you nothing about the skill: the harness reads only the runs
where the two conditions disagreed, and it says so when there were none. If every
skill passes the first time you measure it, the task set is too easy rather than the
library being good.

## Why the holdout exists

A skill written against its own tasks passes its own tasks. `holdout.txt` is the half
someone other than the skill's author writes, and the report scores it separately, so
a skill that helps on one half and does nothing on the other is visible rather than
averaged away.

## Why these files are not in the pack

The agent can read a skill's own directory through `skill_resource`. A task set inside
the pack would be readable by the run being measured, which turns the eval into a
target. Nothing here ships in the binary.
