"""The document table. Rows are dicts: id, owner, title, body, shared_with, updated,
internal_notes.

Reads go through [readable] and [get_for], so a new entry point cannot return a row
without the check. The raw table is not exported.
"""

DOCS = {}


def readable(user, doc):
    return user["role"] == "admin" or doc["owner"] == user["id"] or user["id"] in doc["shared_with"]


def get_for(user, doc_id):
    """The document if it exists and user may read it, else None; the two look the same."""
    doc = DOCS.get(doc_id)
    if doc is None or not readable(user, doc):
        return None
    return doc


def visible_to(user):
    return [d for d in DOCS.values() if readable(user, d)]
