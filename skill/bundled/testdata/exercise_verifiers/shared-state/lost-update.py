import hashlib
import importlib.util
import os
import shutil
import subprocess
import sys
import time

sys.path.insert(0, ".")
time.sleep = lambda s: None
fails = []
ORIGINAL = '''"""Reserving stock for an order."""


class OutOfStock(Exception):
    pass


def reserve(store, sku, qty):
    stock, _ = store.get(sku)
    if stock < qty:
        raise OutOfStock(sku)
    store.put(sku, stock - qty)
'''
try:
    got = hashlib.sha256(open("store.py", "rb").read().replace(b"\r\n", b"\n")).hexdigest()
except OSError:
    got = None
if got != "b6156e0b15fb30ca11d71271942da40ec9ca8cc4af3ca9a738f1094912a70a51":
    fails.append("store.py changed; it is the database client and the fix has to work with it as it is")


class Runaway(BaseException):
    pass


def outcome(fn):
    try:
        fn()
        return "ok"
    except Exception as e:
        return type(e).__name__


try:
    import inventory
    import store

    spec = importlib.util.spec_from_file_location("inventory_other_worker", "inventory.py")
    other = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(other)

    def interleaved(stock):
        s = store.Store({"mug": stock})
        real = s.get
        seen = []
        started = []

        def get(key):
            value = real(key)
            if not started:
                started.append(key)
                seen.append(outcome(lambda: other.reserve(s, key, 1)))
            return value

        s.get = get
        seen.append(outcome(lambda: inventory.reserve(s, "mug", 1)))
        return seen, real("mug")[0]

    results, left = interleaved(1)
    if results.count("ok") != 1 or left != 0:
        fails.append("two workers reserving the last mug got %r and left %r; one must succeed and stock end at 0" % (results, left))
    results, left = interleaved(2)
    if results != ["ok", "ok"] or left != 0:
        fails.append("two workers reserving from 2 mugs got %r and left %r; both must succeed and stock end at 0" % (results, left))

    class Jammed(store.Store):
        puts = 0

        def put(self, key, value, expected_version=None):
            Jammed.puts += 1
            if Jammed.puts > 50:
                raise Runaway()
            raise store.Conflict(key)

    try:
        inventory.reserve(Jammed({"mug": 5}), "mug", 1)
        fails.append("reserve reported success against a store that refused every write")
    except Runaway:
        fails.append("reserve retried a conflict more than 50 times; the retry needs a bound")
    except store.Conflict:
        pass
    except Exception as e:
        fails.append("reserve gave up with %r; exhaustion should still match store.Conflict so a caller can tell it was contention" % (e,))
except Exception as e:
    fails.append("reserve could not be exercised: %r" % (e,))


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, text=True, timeout=60)
    return p.returncode, p.stdout + p.stderr


rc, out = run(".")
if rc != 0:
    fails.append("the tests fail on the result: " + out[-300:])
d = "_mut_original"
shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
with open(os.path.join(d, "inventory.py"), "w", newline="\n") as f:
    f.write(ORIGINAL)
rc, out = run(d)
if rc == 0 or "AssertionError" not in out:
    fails.append("the tests do not fail with an assertion on the original reserve; the fix ships with a test that fails without it")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
