import json
import os

import handlers


def dispatch(name, payload):
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "events.json")
    with open(path, encoding="utf-8") as fh:
        known = json.load(fh)
    if name not in known:
        raise ValueError("unknown event %r" % name)
    return getattr(handlers, "on_" + name)(payload)
