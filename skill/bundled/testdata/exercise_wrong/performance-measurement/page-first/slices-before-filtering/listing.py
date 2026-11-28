"""One page of a workspace's documents."""


def visible_page(documents, viewer, page, size, preview):
    """Return rows for page number page (from 0) of the documents viewer may see,
    newest first. preview(document) renders the preview text and is slow.
    """
    docs = sorted(documents, key=lambda d: d["updated"], reverse=True)
    chosen = [d for d in docs[page * size:(page + 1) * size] if viewer in d["readers"]]
    return [{"id": d["id"], "preview": preview(d)} for d in chosen]
