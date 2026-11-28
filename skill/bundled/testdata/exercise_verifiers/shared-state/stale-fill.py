import importlib.util, sys, threading
sys.path.insert(0, ".")
fails = []
try:
    from cache import Cache

    def load(alias):
        spec = importlib.util.spec_from_file_location(alias, "profiles.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    here, there = load("web_one"), load("web_two")

    class DB:
        def __init__(self):
            self.row = {"id": 1, "name": "ann", "version": 1}
            self.reads = 0
            self.during = None

        def read(self, user_id):
            self.reads += 1
            row = dict(self.row)
            hook, self.during = self.during, None
            if hook:
                hook()
            return row

        def write(self, user_id, name):
            self.row = {"id": 1, "name": name, "version": self.row["version"] + 1}
            return dict(self.row)

    db, cache = DB(), Cache()

    def rename_elsewhere():
        worker = threading.Thread(target=there.update_name, args=(db, cache, 1, "bea"), daemon=True)
        worker.start()
        worker.join(10)
        if worker.is_alive():
            fails.append("a rename blocks while another process is reading")

    db.during = rename_elsewhere
    here.get_profile(db, cache, 1)
    got = here.get_profile(db, cache, 1)["name"]
    if got != "bea":
        fails.append("a rename during another process's read leaves the cache serving %r" % got)
    there.update_name(db, cache, 1, "cy")
    got = here.get_profile(db, cache, 1)["name"]
    if got != "cy":
        fails.append("after a plain rename the profile reads %r" % got)
    before = db.reads
    for _ in range(5):
        here.get_profile(db, cache, 1)
    if db.reads - before > 1:
        fails.append("five reads of one profile read the database %d times" % (db.reads - before))
except Exception as e:
    fails.append("profiles could not be exercised: %r" % e)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
