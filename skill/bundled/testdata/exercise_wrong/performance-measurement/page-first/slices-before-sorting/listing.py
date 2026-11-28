"""One page of a workspace's documents."""


def visible_page(documents, viewer, page, size, preview):
    """Return rows for page number page (from 0) of the documents viewer may see,
    newest first. preview(document) renders the preview text and is slow.
    """
    docs = [d for d in documents if viewer in d["readers"]]
    chosen = sorted(docs[page * size:(page + 1) * size], key=lambda d: d["updated"], reverse=True)
    return [{"id": d["id"], "preview": preview(d)} for d in chosen]
