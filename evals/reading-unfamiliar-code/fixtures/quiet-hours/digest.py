import json
import sys

import dispatch


def flush(users, events, out):
    by_user = {}
    for e in events:
        by_user.setdefault(e["user"], []).append(e)
    for uid in sorted(by_user):
        evs = by_user[uid]
        user = users[uid]
        msg = {"text": "%d updates" % len(evs), "at": evs[-1]["at"], "priority": "normal"}
        for channel in user["channels"]:
            if channel == "inapp":
                continue
            line = dispatch.send(channel, user, msg)
            if line is not None:
                out.append(line)


def main(argv):
    with open(argv[1], encoding="utf-8") as f:
        users = json.load(f)
    with open(argv[2], encoding="utf-8") as f:
        events = [json.loads(line) for line in f if line.strip()]
    out = []
    flush(users, events, out)
    for line in out:
        print(line)


if __name__ == "__main__":
    main(sys.argv)
