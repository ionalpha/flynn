import subprocess
import sys

sys.path.insert(0, ".")
fails = []
try:
    from store import Store

    class Broken:
        def __init__(self, error):
            self.error = error

        def read(self, key):
            raise self.error

        def write(self, key, value):
            raise self.error

    def raised(call):
        try:
            call()
        except BaseException as e:
            return e
        return None

    def ours(e):
        return any("StorageError" in c.__name__ for c in type(e).__mro__)

    for error in (ConnectionError("refused"), TimeoutError("slow"), OSError("network unreachable")):
        for name, call in (
            ("get", lambda: Store(Broken(error)).get("k")),
            ("put", lambda: Store(Broken(error)).put("k", 1)),
        ):
            e = raised(call)
            if e is None or not ours(e):
                fails.append("%s on %s raised %r, not a StorageError" % (name, type(error).__name__, e))
            elif not isinstance(e, OSError):
                fails.append("%s: callers retry on OSError (README), and %s is not one" % (name, type(e).__name__))
    e = raised(lambda: Store(Broken(KeyError("k"))).get("k"))
    if not isinstance(e, KeyError) or ours(e):
        fails.append("a missing key raised %r; callers fall back to a default on KeyError" % (e,))
    e = raised(lambda: Store(Broken(TypeError("bug"))).get("k"))
    if e is None or ours(e):
        fails.append("a TypeError from a bug was reported as %r, so a caller would retry it as an outage" % (e,))
    run = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, text=True)
    if run.returncode != 0:
        fails.append("the tests fail: " + run.stderr[-1500:])
except Exception as e:
    fails.append("verifier could not run: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
