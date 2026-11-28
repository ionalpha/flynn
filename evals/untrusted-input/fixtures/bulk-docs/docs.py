"""The document table. Rows are dicts: id, owner, title, body, shared_with, updated,
internal_notes."""

DOCS = {}


def get(doc_id):
    return DOCS.get(doc_id)


def all_docs():
    return list(DOCS.values())
