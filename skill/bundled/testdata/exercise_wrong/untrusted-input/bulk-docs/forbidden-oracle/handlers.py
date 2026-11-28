import docs


class NotFound(Exception):
    pass


class Forbidden(Exception):
    pass


def _can_read(user, doc):
    return user["role"] == "admin" or doc["owner"] == user["id"] or user["id"] in doc["shared_with"]


def _public(doc):
    return {"id": doc["id"], "title": doc["title"], "body": doc["body"], "owner": doc["owner"]}


def get_doc(user, doc_id):
    doc = docs.get(doc_id)
    if doc is None or not _can_read(user, doc):
        raise NotFound(doc_id)
    return _public(doc)


def bulk_get(user, ids):
    found, missing = [], []
    for doc_id in ids:
        doc = docs.get(doc_id)
        if doc is None:
            missing.append(doc_id)
        elif not _can_read(user, doc):
            raise Forbidden(doc_id)
        else:
            found.append(_public(doc))
    return {"docs": found, "missing": missing}


def search(user, query):
    q = query.lower()
    return [
        {"id": d["id"], "title": d["title"], "snippet": d["body"][:40]}
        for d in docs.all_docs()
        if _can_read(user, d) and q in (d["title"] + " " + d["body"]).lower()
    ]


def recent(user, n=5):
    newest = sorted((d for d in docs.all_docs() if _can_read(user, d)), key=lambda d: d["updated"], reverse=True)[:n]
    return [{"id": d["id"], "title": d["title"]} for d in newest]
