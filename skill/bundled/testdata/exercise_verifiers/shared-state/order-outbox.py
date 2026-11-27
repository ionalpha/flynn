import os, shutil, sqlite3, sys, tempfile
sys.path.insert(0, ".")
fails = []
d = tempfile.mkdtemp(dir=".")
try:
    import app

    class Kill(BaseException):
        pass

    class Bus:
        def __init__(self):
            self.sent = []
            self.fail = None
            self.room = None

        def publish(self, m):
            if self.fail:
                raise self.fail
            if self.room is not None:
                if self.room == 0:
                    raise ConnectionError("down")
                self.room -= 1
            self.sent.append(m["id"])

    path = os.path.join(d, "shop.db")
    old = sqlite3.connect(path)
    old.execute("CREATE TABLE orders (id TEXT PRIMARY KEY, total INTEGER NOT NULL)")
    old.execute("INSERT INTO orders VALUES ('old', 1)")
    old.commit()
    old.close()

    conns = []

    def start():
        for c in conns:
            c.close()
        c = sqlite3.connect(path)
        conns[:] = [c]
        app.init(c)
        return c

    def call(fn, *args):
        try:
            fn(*args)
        except (Exception, Kill):
            pass

    bus = Bus()
    c = start()
    for oid, failure in (("down", ConnectionError("down")), ("killed", Kill())):
        bus.fail = failure
        call(app.place_order, c, bus, oid, 5)
        c = start()
        bus.fail = None
        app.relay(c, bus)
        saved = c.execute("SELECT count(*) FROM orders WHERE id = ?", (oid,)).fetchone()[0]
        if saved and oid not in bus.sent:
            fails.append("order %s was saved and never announced" % oid)
        if not saved and oid in bus.sent:
            fails.append("order %s was announced and never saved" % oid)
        if not saved:
            fails.append("order %s was not saved, though only the broker or the process failed" % oid)

    app.place_order(c, bus, "fine", 5)
    app.relay(c, bus)
    if "fine" not in bus.sent:
        fails.append("an ordinary order was not announced")
    count = len(bus.sent)
    call(app.place_order, c, bus, "fine", 9)
    c = start()
    app.relay(c, bus)
    app.relay(c, bus)
    if len(bus.sent) != count:
        fails.append("orders already announced were announced again: %r" % bus.sent[count:])

    bus.fail = ConnectionError("down")
    for oid in ("p1", "p2", "p3"):
        call(app.place_order, c, bus, oid, 1)
    bus.fail, bus.room = None, 2
    call(app.relay, c, bus)
    bus.room = None
    c = start()
    app.relay(c, bus)
    lost = [o for o in ("p1", "p2", "p3") if o not in bus.sent]
    if lost:
        fails.append("after the broker dropped during a relay run these were never announced: %r" % lost)
    for c in conns:
        c.close()
except Exception as e:
    fails.append("checkout could not be exercised: %r" % e)
shutil.rmtree(d, ignore_errors=True)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
