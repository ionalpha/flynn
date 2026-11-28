import sys
sys.path.insert(0, ".")
fails = []
try:
    from dashboard import render

    def task(title, owner, private):
        return {"title": title, "owner": owner, "private": private, "status": "open"}

    class DB:
        def __init__(self):
            self.rows = {1: [task("a", "ann", False), task("b", "ann", True), task("c", "bob", True)], 2: [task("z", "cat", False)]}
            self.versions = {1: 1, 2: 1}
            self.reads = 0

        def tasks(self, p):
            self.reads += 1
            return [dict(t) for t in self.rows[p]]

        def version(self, p):
            return self.versions[p]

        def update(self, p, rows):
            self.rows[p] = rows
            self.versions[p] += 1

    db = DB()
    admin, ann, bob, eve = ({"id": n, "admin": n == "root"} for n in ("root", "ann", "bob", "eve"))
    want = [(admin, 1, "abc"), (ann, 1, "ab"), (bob, 1, "ac"), (eve, 1, "a"), (admin, 2, "z")]
    for _ in range(2):
        for viewer, p, titles in want:
            got = "".join(render(viewer, p, db)["titles"])
            if got != titles:
                fails.append("%s on project %d sees %r, want %r" % (viewer["id"], p, got, titles))
    before = db.reads
    for viewer, p, _ in want:
        render(viewer, p, db)
    if db.reads != before:
        fails.append("repeat views still read the tasks %d times" % (db.reads - before))
    db.update(1, [task("n", "bob", True)])
    for viewer, titles in ((admin, "n"), (bob, "n"), (ann, "")):
        got = "".join(render(viewer, 1, db)["titles"])
        if got != titles:
            fails.append("after a change %s sees %r, want %r" % (viewer["id"], got, titles))
    demoted = {"id": "root", "admin": False}
    if "".join(render(demoted, 1, db)["titles"]) != "":
        fails.append("someone who stopped being an admin still sees the admin view")
except Exception as e:
    fails.append("render could not be exercised: %r" % e)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
