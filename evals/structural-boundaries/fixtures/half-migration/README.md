# reports

Scheduled reports. `python3 runner.py <job>` prints one report; `jobs.json` maps each
job name to the function that builds it. CI runs `python3 -m unittest discover`.
See `MIGRATION.md` before touching how a report is rendered.
