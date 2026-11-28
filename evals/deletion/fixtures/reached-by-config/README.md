# plumbing

`export.py` renders rows in the format a caller names; `events.py` dispatches billing
events to handlers; `jobs.py` holds the scheduled jobs that `crontab.txt` runs.
Configuration is in `formats.json`, `events.json` and `crontab.txt`.
CI runs `python3 -m unittest discover`.
