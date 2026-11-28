import json
import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
ORIGINAL = '''import time


class Replica:
    """One device's copy of the settings. clock() returns seconds."""

    def __init__(self, node, clock=time.time):
        self.node = node
        self.clock = clock
        self.data = {}

    def set(self, key, value):
        self.data[key] = (self.clock(), value)

    def get(self, key, default=None):
        item = self.data.get(key)
        return item[1] if item else default

    def snapshot(self):
        """The state as JSON-ready data: {key: [stamp, value]}."""
        return {k: [stamp, value] for k, (stamp, value) in self.data.items()}

    def merge(self, snapshot):
        for key, (stamp, value) in snapshot.items():
            mine = self.data.get(key)
            if mine is None or stamp > mine[0]:
                self.data[key] = (stamp, value)
'''


class Clock:
    def __init__(self, now):
        self.now = now

    def __call__(self):
        return self.now


def trip(snapshot):
    return json.loads(json.dumps(snapshot))


try:
    from replica import Replica

    # a device whose clock runs 3 seconds behind sees a write, then answers it
    a, b = Replica("n2", Clock(1000.0)), Replica("n1", Clock(997.0))
    a.set("theme", "dark")
    b.merge(trip(a.snapshot()))
    b.set("theme", "light")
    a.merge(trip(b.snapshot()))
    b.merge(trip(a.snapshot()))
    if (a.get("theme"), b.get("theme")) != ("light", "light"):
        fails.append("a write made after seeing 'dark' on a slow clock gave %r and %r; the later write must win everywhere" % (a.get("theme"), b.get("theme")))

    # a clock that steps back
    a, b = Replica("n1", Clock(1000.0)), Replica("n2", Clock(1000.0))
    a.set("k", "v1")
    a.clock.now = 990.0
    a.set("k", "v2")
    b.merge(trip(a.snapshot()))
    if b.get("k") != "v2":
        fails.append("after the clock stepped back the second write read %r elsewhere, want 'v2'" % (b.get("k"),))

    # two writes in one clock reading, snapshots arriving in order
    a, b = Replica("n1", Clock(500.0)), Replica("n2", Clock(500.0))
    a.set("k", 1)
    s1 = trip(a.snapshot())
    a.set("k", 2)
    s2 = trip(a.snapshot())
    b.merge(s1)
    b.merge(s2)
    if b.get("k") != 2:
        fails.append("two writes in the same clock reading left %r after merging in order, want 2" % (b.get("k"),))

    # concurrent writes, same reading: every order of merging ends the same
    a, b = Replica("n1", Clock(500.0)), Replica("n2", Clock(500.0))
    a.set("x", "from-a")
    b.set("x", "from-b")
    sa, sb = trip(a.snapshot()), trip(b.snapshot())
    ends = []
    for order in ((sa, sb), (sb, sa), (sa, sb, sa), (sb, sa, sb, sa)):
        c = Replica("n3", Clock(500.0))
        for s in order:
            c.merge(s)
        ends.append(c.get("x"))
    a.merge(sb)
    b.merge(sa)
    ends += [a.get("x"), b.get("x")]
    if len(set(ends)) != 1:
        fails.append("concurrent writes ended as %r depending on the order they were merged; every device must agree" % (ends,))

    # merging the same snapshot again changes nothing
    c = Replica("n3", Clock(600.0))
    c.merge(sa)
    c.merge(sb)
    before = c.snapshot()
    c.merge(sa)
    c.merge(sb)
    if c.snapshot() != before:
        fails.append("merging a snapshot already merged changed the state")

    # a snapshot written before the change, with plain seconds for stamps
    old = json.load(open("data/phone.json", encoding="utf-8"))
    for label, now in (("a clock after it", 1700000300.0), ("a clock behind it", 1699990000.0)):
        c = Replica("n1", Clock(now))
        c.merge(old)
        if (c.get("theme"), c.get("language")) != ("dark", "en"):
            fails.append("the old snapshot did not load: %r" % ((c.get("theme"), c.get("language")),))
        c.set("theme", "light")
        d = Replica("n2", Clock(1700000050.0))
        d.merge(old)
        d.merge(trip(c.snapshot()))
        if d.get("theme") != "light":
            fails.append("with %s, a new write lost to the old snapshot's stamp: got %r" % (label, d.get("theme")))
except Exception as e:
    fails.append("replica could not be exercised: %r" % (e,))


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stdout + p.stderr


rc, out = run(".")
if rc != 0:
    fails.append("the tests fail on the result: " + out[-300:])
d = "_mut_original"
shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git", ".flynn-ab"))
with open(os.path.join(d, "replica.py"), "w", newline="\n", encoding="utf-8") as f:
    f.write(ORIGINAL)
rc, out = run(d)
shutil.rmtree(d, ignore_errors=True)
if rc == 0 or "AssertionError" not in out:
    fails.append("the tests do not fail with an assertion on the original replica; the fix ships with a test that fails without it")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
