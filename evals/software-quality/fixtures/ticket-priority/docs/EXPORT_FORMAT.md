# Nightly ticket export

`export.write` produces `outbox/tickets.csv` every night. Partners pick it up and
feed it to their own jobs.

| Column | Meaning |
|---|---|
| id | ticket number |
| title | one line of text |
| status | `open` or `closed` |
| opened | date the ticket was opened, `YYYY-MM-DD` |

Partner jobs are written against this layout and are deployed on their schedule,
not ours. The file has a header row. Columns are only ever added at the end; an
existing column keeps its position and its meaning.
