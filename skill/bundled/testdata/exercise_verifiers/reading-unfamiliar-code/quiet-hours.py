import json
import subprocess
import sys

fails = []
USERS = {
    "u1": {"name": "ana", "channels": ["mobile", "email"], "utc_offset_min": 330, "quiet": [22, 7]},
    "u2": {"name": "bo", "channels": ["watch", "inapp"], "utc_offset_min": -480, "quiet": [22, 7]},
    "u3": {"name": "cy", "channels": ["mobile"], "utc_offset_min": 0},
    "u4": {"name": "di", "channels": ["mobile", "watch", "email"], "utc_offset_min": 60, "quiet": [13, 15]},
}
ALL = {u: v["channels"] for u, v in USERS.items()}
# (user, UTC time, priority, channels that must deliver); the local hour is in the comment.
CASES = [
    ("u1", "2024-03-05T17:00Z", "normal", ["email"]),  # 22:30 local, quiet
    ("u1", "2024-03-05T01:30Z", "normal", ALL["u1"]),  # 07:00 local, the window ends before 7
    ("u1", "2024-03-05T17:00Z", "urgent", ALL["u1"]),  # quiet but urgent
    ("u2", "2024-03-05T07:00Z", "normal", ["inapp"]),  # 23:00 local the day before
    ("u3", "2024-03-05T03:00Z", "normal", ["mobile"]),  # no quiet hours set
    ("u4", "2024-03-05T12:00Z", "normal", ["email"]),  # 13:00 local, a window that does not wrap
    ("u4", "2024-03-05T13:59Z", "normal", ["email"]),  # 14:59 local
    ("u4", "2024-03-05T14:00Z", "normal", ALL["u4"]),  # 15:00 local
    ("u1", "2024-03-05T16:29Z", "normal", ALL["u1"]),  # 21:59 local
    ("u2", "2024-03-05T15:59Z", "normal", ALL["u2"]),  # 07:59 local
]
DIGEST = [("u1", "2024-03-05T17:00Z"), ("u1", "2024-03-05T17:10Z"), ("u2", "2024-03-05T07:00Z"), ("u4", "2024-03-05T14:00Z")]


def run(script, events):
    with open("_v_users.json", "w", encoding="utf-8") as f:
        json.dump(USERS, f)
    with open("_v_events.jsonl", "w", encoding="utf-8") as f:
        for e in events:
            f.write(json.dumps(e) + "\n")
    p = subprocess.run([sys.executable, script, "_v_users.json", "_v_events.jsonl"], capture_output=True,
                       encoding="utf-8", errors="replace", timeout=30)
    if p.returncode != 0:
        fails.append("%s failed: %s" % (script, p.stderr[-300:]))
    return p.stdout.splitlines()


events, want = [], []
for i, (u, at, pr, chans) in enumerate(CASES):
    events.append({"user": u, "kind": "order.shipped", "order": "o%d" % i, "at": at, "priority": pr})
    want += ["%s %s Order o%d shipped" % (c, USERS[u]["name"], i) for c in chans]
got = run("app.py", events)
if got != want:
    fails.append("app.py delivered %r, want %r" % (got, want))

evs = [{"user": u, "kind": "order.shipped", "order": "x", "at": at} for u, at in DIGEST]
want = ["email ana 2 updates", "mobile di 1 updates", "watch di 1 updates", "email di 1 updates"]
got = run("digest.py", evs)
if got != want:
    fails.append("digest.py delivered %r, want %r" % (got, want))

p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                   capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the project's own tests fail: " + p.stderr[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
