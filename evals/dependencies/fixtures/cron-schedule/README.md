# reports

Sends the scheduled reports. Each report has a cron expression (five fields, the
meaning `crontab(5)` gives them: when both day-of-month and day-of-week are restricted,
a day matches if either does). The scheduler asks `schedule.next_run(expr, after)` for
the first minute after `after` at which the expression fires.

Released under the MIT licence (see `LICENSE`) and shipped to customers as a packaged
program.

## Dependencies

Runtime dependencies are listed in `deps.txt`, one per line, and installed by the
packaging script. This sandbox has no network, so the packages a change might use are
already unpacked under `pylib/`; run anything with `PYTHONPATH=pylib`. `REGISTRY.md` is
a snapshot of what the registry shows for them.
