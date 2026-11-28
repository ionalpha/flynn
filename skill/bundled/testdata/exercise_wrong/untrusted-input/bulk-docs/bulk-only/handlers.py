import docs


class NotFound(Exception):
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
        if doc is None or not _can_read(user, doc):
            missing.append(doc_id)
        else:
            found.append(_public(doc))
    return {"docs": found, "missing": missing}


def search(user, query):
    q = query.lower()
    return [
        {"id": d["id"], "title": d["title"], "snippet": d["body"][:40]}
        for d in docs.all_docs()
        if q in (d["title"] + " " + d["body"]).lower()
    ]


def recent(user, n=5):
    newest = sorted(docs.all_docs(), key=lambda d: d["updated"], reverse=True)[:n]
    return [{"id": d["id"], "title": d["title"]} for d in newest]
