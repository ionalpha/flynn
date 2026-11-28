import base64
import json
import sys
import time
from urllib.parse import parse_qs, urlparse

sys.path.insert(0, ".")
fails = []
time.sleep = lambda seconds: None


def row(i, v=1):
    return {"id": "c%03d" % i, "version": v}


pages = [
    [row(i) for i in range(1, 51)],
    [],
    [row(i, 2 if i == 60 else 1) for i in range(51, 81)] + [row(20, 2)],
    [row(60, 1)] + [row(i) for i in range(81, 130)],
    [row(i) for i in range(130, 138)],
]
tokens = [None] + [
    base64.b64encode(b"\xfb\xff\xfe" + ("page-%d" % i).encode() + b"!" * i).decode() for i in range(1, 5)
]
calls = {}
mode = {"cursor": None, "times": 0}

try:
    import sync
    import transport

    def get(url, headers=None):
        u = urlparse(url)
        q = parse_qs(u.query)
        if u.path != "/v2/customers":
            return transport.Response(404, {}, "{}")
        if int(q.get("limit", ["20"])[0]) > 50:
            return transport.Response(400, {}, '{"error": "limit"}')
        cur = q.get("cursor", [None])[0]
        if cur not in tokens:
            return transport.Response(400, {}, '{"error": "invalid cursor"}')
        calls[cur] = calls.get(cur, 0) + 1
        if cur == mode["cursor"] and (mode["times"] is None or calls[cur] <= mode["times"]):
            return transport.Response(503, {}, "<html>unavailable</html>")
        i = tokens.index(cur)
        nxt = tokens[i + 1] if i + 1 < len(tokens) else None
        return transport.Response(200, {}, json.dumps({"data": pages[i], "next_cursor": nxt}))

    transport.get = get

    def reset(cursor=None, times=0):
        calls.clear()
        mode["cursor"], mode["times"] = cursor, times

    def run():
        try:
            return sync.sync_customers(), None
        except Exception as e:
            return None, e

    def check(got, label):
        ids = [r["id"] for r in got]
        want = {"c%03d" % i for i in range(1, 138)}
        if set(ids) != want:
            fails.append("%s: got %d distinct customers, want 137 (missing %s)" % (label, len(set(ids)), sorted(want - set(ids))[:4]))
        if len(ids) != len(set(ids)):
            fails.append("%s: a customer that appeared on two pages was returned twice" % label)
        by = {r["id"]: r["version"] for r in got}
        if by.get("c020") != 2 or by.get("c060") != 2:
            fails.append("%s: a customer seen twice must keep the copy with the higher version (c020=%r, c060=%r)" % (label, by.get("c020"), by.get("c060")))

    reset()
    got, err = run()
    if got is None:
        fails.append("a clean listing raised %r" % (err,))
    else:
        check(got, "clean listing")

    reset(tokens[1], 1)
    got, err = run()
    if got is None:
        fails.append("one 503 on a page raised %r; the provider says it clears in seconds" % (err,))
    else:
        check(got, "one 503")

    reset(tokens[3], None)
    got, err = run()
    if got is not None:
        fails.append("the CRM stayed down from page 4 and sync_customers returned %d customers as if the sync had finished" % len(got))
    if calls.get(tokens[3], 0) > 3:
        fails.append("the provider allows 3 attempts per request, a page that stayed down was asked %d times" % calls[tokens[3]])
except Exception as e:
    fails.append("the sync could not be exercised: %r" % (e,))
for f in dict.fromkeys(fails):
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
