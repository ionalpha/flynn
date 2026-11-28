# sessionkit

Session upkeep, published to other teams as the `sessionkit` package (version 2.4.0).
The public API is whatever `sessions.__all__` lists; `jobs.py` and `maintenance.py` are
this repository's own schedulers.

`sessions.purge_old_sessions(now, days=30)` removes sessions created more than `days` days
before `now` (an ISO date) and returns how many it removed.

Deprecated names stay for at least one major version and warn with the version that
removes them. The warning names the replacement too.

A public function is named for what it does to the data: `purge_*` destroys rows, `archive_*`
moves them to the matching `*_archive` table. When a change of behaviour would make a name
untrue, publish the function under a true name and deprecate the old one.

Run the tests with `python3 -m unittest discover`.
