# Weekly ledger report

`report.build(rows)` turns an export into the lines of the weekly report. Production
exports run to about 20,000 rows, roughly half of them repeats of an earlier row,
because the bank feed resends corrections.

`python3 bench.py` is what CI runs against a 40 row sample and it passes with room to
spare. The job on the full export is what people are waiting on.
