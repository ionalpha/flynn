import sys

sys.path.insert(0, ".")
fails = []
try:
    import docs
    import handlers

    def doc(i, owner, title, body, shared=(), updated=1):
        docs.DOCS[i] = {"id": i, "owner": owner, "title": title, "body": body, "shared_with": list(shared), "updated": updated, "internal_notes": "n"}

    doc("a1", "ann", "alpha plan", "alpha roadmap", updated=1)
    doc("b1", "bob", "beta budget", "beta salary table", updated=9)
    doc("s1", "bob", "shared budget notes", "for ann too", shared=["ann"], updated=5)
    ann = {"id": "ann", "role": "member"}
    root = {"id": "root", "role": "admin"}

    def ids(rows):
        return sorted(r["id"] for r in rows)

    def attempt(name, fn):
        try:
            return fn()
        except Exception as e:
            fails.append("%s raised %r for a legitimate request" % (name, e))
            return None

    try:
        handlers.get_doc(ann, "b1")
        fails.append("get_doc returned another user's document")
    except handlers.NotFound:
        pass
    if attempt("get_doc on a shared document", lambda: handlers.get_doc(ann, "s1")) is None:
        pass

    out = attempt("bulk_get", lambda: handlers.bulk_get(ann, ["a1", "b1", "s1", "nope"]))
    if out is not None:
        if ids(out["docs"]) != ["a1", "s1"]:
            fails.append("bulk_get gave ann the documents %r; she may read a1 and s1" % ids(out["docs"]))
        if sorted(out["missing"]) != ["b1", "nope"]:
            fails.append("bulk_get reports missing %r; a document she may not read must look the same as one that does not exist" % sorted(out["missing"]))
        if "salary" in repr(out):
            fails.append("bulk_get output carries text from bob's private document")
    one = attempt("bulk_get on one forbidden id", lambda: handlers.bulk_get(ann, ["b1"]))
    two = attempt("bulk_get on one unknown id", lambda: handlers.bulk_get(ann, ["zz9"]))
    if one is not None and two is not None:
        if one["docs"] or one["missing"] != ["b1"] or two["docs"] or two["missing"] != ["zz9"]:
            fails.append("a forbidden id and an unknown id are answered differently by bulk_get")

    res = attempt("search", lambda: handlers.search(ann, "budget"))
    if res is not None and ids(res) != ["s1"]:
        fails.append("search for 'budget' as ann returned %r; only s1 is hers to read" % ids(res))
    res = attempt("search", lambda: handlers.search(ann, "salary"))
    if res:
        fails.append("search matched the text of a document ann may not read: %r" % res)
    res = attempt("recent", lambda: handlers.recent(ann))
    if res is not None and ids(res) != ["a1", "s1"]:
        fails.append("recent for ann lists %r; she may see a1 and s1" % ids(res))

    out = attempt("bulk_get as admin", lambda: handlers.bulk_get(root, ["a1", "b1", "s1"]))
    if out is not None and ids(out["docs"]) != ["a1", "b1", "s1"]:
        fails.append("an admin lost access: bulk_get gave %r" % ids(out["docs"]))
    res = attempt("search as admin", lambda: handlers.search(root, "budget"))
    if res is not None and ids(res) != ["b1", "s1"]:
        fails.append("an admin searching 'budget' got %r" % ids(res))
except Exception as e:
    fails.append("the document handlers could not be exercised: %r" % (e,))
fails = list(dict.fromkeys(fails))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
