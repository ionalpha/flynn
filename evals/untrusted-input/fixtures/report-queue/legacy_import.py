"""Nightly import of a partner's CSV export into the report queue."""

import queue_store


def import_rows(rows):
    """rows are dicts with the title and body columns of the partner's file."""
    for row in rows:
        queue_store.put({"kind": "report", "owner": "import", "title": row["title"], "body": row["body"]})
