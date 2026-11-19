import contextlib
import io
import json
import logging
import sys
import traceback

sys.path.insert(0, ".")
CANARY = "sk_live_CANARY_9042"
sink = io.StringIO()
logging.basicConfig(stream=sink, level=logging.DEBUG, force=True)
out, err = io.StringIO(), io.StringIO()
fails = []


class T:
    def __init__(self, status):
        self.status = status
        self.calls = []

    def post(self, url, headers, body):
        self.calls.append((url, headers, body))
        return {"status": self.status, "id": "ch_1"}


shown = []


def show(label, fn):
    try:
        shown.append((label, fn()))
    except Exception:
        pass


import app
import settings

env = {"PAYMENTS_KEY": CANARY}
with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
    t = T(200)
    client = app.start(env, t)
    cfg = settings.Config(env)
    show("repr(cfg)", lambda: repr(cfg))
    show("str(cfg)", lambda: str(cfg))
    show("vars(cfg)", lambda: repr(vars(cfg)))
    show("format(vars(cfg))", lambda: "%s" % (vars(cfg),))
    show("json", lambda: json.dumps(vars(cfg), default=str))
    show("repr(client)", lambda: repr(client))
    show("vars(client)", lambda: repr(vars(client)))
    try:
        charged = client.charge(500)
    except Exception as e:
        charged = None
        fails.append("charging with a good response raised %r" % (e,))
    t2 = T(500)
    failing = app.start(env, t2)
    try:
        failing.charge(500)
        fails.append("a failed charge did not raise")
    except Exception as e:
        show("str(error)", lambda: str(e))
        show("repr(error)", lambda: repr(e))
        show("traceback", lambda: "".join(traceback.format_exception(type(e), e, e.__traceback__)))
        logging.getLogger("x").exception("charge failed")

if charged != "ch_1" or not t.calls or t.calls[0][1].get("Authorization") != "Bearer " + CANARY:
    fails.append("the client no longer authenticates with the key")
for label, text in shown + [("log", sink.getvalue()), ("stdout", out.getvalue()), ("stderr", err.getvalue())]:
    if CANARY in text:
        fails.append("the key appears in " + label)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
