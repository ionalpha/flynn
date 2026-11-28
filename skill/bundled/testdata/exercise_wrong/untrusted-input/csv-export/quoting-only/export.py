import csv
import io

COLUMNS = ["id", "name", "rating", "comment"]


def to_csv(rows):
    """CSV text of the feedback rows for support to download."""
    out = io.StringIO()
    writer = csv.writer(out)
    writer.writerow(COLUMNS)
    for row in rows:
        writer.writerow([row[c] for c in COLUMNS])
    return out.getvalue()
