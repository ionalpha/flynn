import sys, threading, time
sys.path.insert(0, ".")
fails = []
try:
    from vendor.profiles import NotFound, TooBusy
    from report import load_profiles

    class Fake:
        def __init__(self):
            self.lock = threading.Lock()
            self.now = self.peak = self.refused = 0

        def get(self, user_id):
            with self.lock:
                if self.now >= 8:
                    self.refused += 1
                    raise TooBusy(user_id)
                self.now += 1
                self.peak = max(self.peak, self.now)
            try:
                time.sleep(0.002 + (user_id % 7) * 0.004)
                if user_id < 0:
                    raise NotFound(user_id)
                return {"id": user_id}
            finally:
                with self.lock:
                    self.now -= 1

    ids = [(i * 37) % 101 for i in range(120)] + [5, 5]
    fake = Fake()
    got = [p["id"] for p in load_profiles(fake, ids)]
    if got != ids:
        fails.append("profiles do not come back one per id in the order given")
    if fake.refused:
        fails.append("the service refused %d requests as over its limit" % fake.refused)
    if fake.peak < 4:
        fails.append("at most %d requests were in flight, so the wait is still one at a time" % fake.peak)
    try:
        load_profiles(Fake(), [1, -1, 2])
        fails.append("an unknown id no longer raises NotFound")
    except NotFound:
        pass
except Exception as e:
    fails.append("load_profiles could not be exercised: %r" % e)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
