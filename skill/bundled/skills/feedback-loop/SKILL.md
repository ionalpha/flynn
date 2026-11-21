---
name: feedback-loop
description: Use when the check for a change is slow, unclear, flaky or done by hand; when you have run the same command, repeated the same setup or read the same output by eye three times; and before planning a change, to say how its result will be known and how long that takes. Covers timing the loop from an edit to a verdict, building the smallest script, fixture, query or trace that shortens it, keeping it where the next run finds it, and when not to build one.
metadata:
  flynnhq.com/title: Feedback loop
  flynnhq.com/tags: '["feedback loop","slow tests","tooling","fixture","script","flaky","iteration speed","developer productivity"]'
---

# Feedback loop

## Time the loop before the first edit

Write down four things: the check that tells you the change worked, the command or steps that run it, how long one run takes from saving an edit to reading the verdict, and which steps need a person (reading a screen, scanning a log, clicking through, copying a value).

The loop needs work when any of these hold:

- **Slow**: you wait on it longer than you work between runs.
- **Unclear**: its output does not say pass or fail for the property you changed.
- **Manual**: any step is done by hand.
- **Flaky**: the same code gives different verdicts.

## Shorten it when the runs left cost more than the tool

Estimate in one line: runs still to come, times the time each run would save, against the time to build the tool. Build when the saving is larger within this task, or when it is close and the tool will be kept for the next person who works here. Otherwise run the slow loop and say so in the report.

Try the cheapest fix first:

1. **Narrow.** Run the one test, package, file or request, using the project's runner, not the whole suite.
2. **Make the verdict explicit.** Replace "look at the output" with a command that exits non-zero and prints the line that differs.
3. **Capture the input.** Save a real failing input, response or record as a fixture, so the loop runs without the service, the screen or the full data set.
4. **Script the manual step.** The setup, requests or clicks you repeated.
5. **Instrument.** When the answer is inside a running system, a log query, trace or counter that answers the question directly.
6. **Fix the flake first.** A loop that gives different verdicts on the same code is not a check; find what varies (time, order, shared state, network) before relying on it.

## The tool is for iterating, not for the report

Before you report, run the project's own check. A tool you built never replaces, weakens or edits an existing test or check. If your tool and the project's check disagree, the project's check stands and the disagreement goes in the report.

## Keep it or remove it

Keep a tool when the next person working here will run the same loop. Put it where the project keeps such things (its scripts, test fixtures or task runner), name it after the question it answers, make it run with one command and no edits, write it in a language and with dependencies the project already has, commit it apart from the change it served, and list it in the report under Changed. A kept tool is code and passes the same checks as the rest of the change.

Remove a tool only this task needed before you report, with any temporary logging.

## Repeated work

When you have done the same sequence by hand three times in one task, script it or write in the report why not. Put any repeated manual step you saw and did not script under "Found on the way".
