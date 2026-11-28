"""The nightly CSV export. Its layout is published in docs/EXPORT_FORMAT.md."""
import csv

import store

COLUMNS = ["id", "title", "status", "opened", "priority"]
DEFAULTS = {"priority": "normal"}


def rows(tickets):
    for t in tickets:
        yield [t.get(c, DEFAULTS.get(c)) if c in DEFAULTS else t[c] for c in COLUMNS]


def write(tickets_path, out_path):
    tickets = store.load(tickets_path)
    with open(out_path, "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f, lineterminator="\n")
        w.writerow(COLUMNS)
        w.writerows(rows(tickets))
