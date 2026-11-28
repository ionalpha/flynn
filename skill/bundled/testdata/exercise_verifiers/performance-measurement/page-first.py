import sys
sys.path.insert(0, ".")
fails = []
try:
    from listing import visible_page
    docs = [{"id": i, "updated": (i * 53) % 211, "readers": ["ann"] if i % 3 else ["bob"]} for i in range(200)]
    mine = sorted((d for d in docs if "ann" in d["readers"]), key=lambda d: d["updated"], reverse=True)
    for page in (0, 1, 6):
        calls = []

        def preview(d):
            calls.append(d["id"])
            return "p%d" % d["id"]

        got = visible_page(list(docs), "ann", page, 20, preview)
        want = [{"id": d["id"], "preview": "p%d" % d["id"]} for d in mine[page * 20:(page + 1) * 20]]
        if got != want:
            fails.append("page %d is not the same rows as before" % page)
        if len(calls) > 20:
            fails.append("page %d rendered %d previews for at most 20 rows" % (page, len(calls)))
except Exception as e:
    fails.append("visible_page could not be exercised: %r" % e)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
