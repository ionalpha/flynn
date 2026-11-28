"""One page of a workspace's documents."""


def visible_page(documents, viewer, page, size, preview):
    """Return rows for page number page (from 0) of the documents viewer may see,
    newest first. preview(document) renders the preview text and is slow.
    """
    rows = []
    for doc in documents:
        rows.append({"id": doc["id"], "preview": preview(doc), "doc": doc})
    rows = [r for r in rows if viewer in r["doc"]["readers"]]
    rows.sort(key=lambda r: r["doc"]["updated"], reverse=True)
    return [{"id": r["id"], "preview": r["preview"]} for r in rows[page * size:(page + 1) * size]]
