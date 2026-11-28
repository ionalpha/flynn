# taskrun

A small command runner. Job files give timeouts and intervals as text such as `1h30m`
or `45s`. `durations.parse_duration(text)` turns that text into seconds and has not been
written yet.

Units are `w`, `d`, `h`, `m` (minutes) and `s`, case-insensitive, optionally with a
decimal amount (`1.5h`) and with spaces between parts (`1h 30m`). Anything else is a
`ValueError`: an empty string, a number with no unit, an unknown unit, a unit given twice.

taskrun is released under the MIT licence (`LICENSE`) and installed by copying its files,
so it has no third-party dependencies and will not gain any.

`sources/` holds snapshots of the three implementations found while searching, as each
page showed them.
