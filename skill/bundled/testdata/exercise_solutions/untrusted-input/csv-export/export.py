import csv
import io

COLUMNS = ["id", "name", "rating", "comment"]


def _inert(value):
    # A spreadsheet evaluates a cell that starts with one of these, quoted or not.
    if isinstance(value, str) and value.startswith(("=", "+", "-", "@", "\t", "\r")):
        return "'" + value
    return value


def to_csv(rows):
    """CSV text of the feedback rows for support to download."""
    out = io.StringIO()
    writer = csv.writer(out)
    writer.writerow(COLUMNS)
    for row in rows:
        writer.writerow([_inert(row[c]) for c in COLUMNS])
    return out.getvalue()
