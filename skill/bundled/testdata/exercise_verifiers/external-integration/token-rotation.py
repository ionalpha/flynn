import importlib
import json
import os
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
try:
    tests = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, text=True)
    if tests.returncode != 0:
        fails.append("the tests fail: " + tests.stderr[-1500:])
    import transport

    class Runaway(BaseException):
        pass

    state = {"n": 0, "access": None, "refresh": "rt0", "revoked": False,
             "forbid": False, "broken": False, "gets": 0, "posts": 0}
    contacts = [{"id": 7}]

    def get(url, headers=None, *args, **kwargs):
        state["gets"] += 1
        if state["gets"] > 20:
            raise Runaway("data calls")
        token = str((headers or {}).get("Authorization", "")).split(" ")[-1]
        if state["forbid"] or state["revoked"] or token != state["access"]:
            return transport.Response(401, {}, json.dumps({"error": "unauthorized"}))
        return transport.Response(200, {}, json.dumps({"contacts": contacts}))

    def post(url, body=None, headers=None, *args, **kwargs):
        body = body if body is not None else kwargs.get("json", kwargs.get("data"))
        if isinstance(body, str):
            body = json.loads(body)
        state["posts"] += 1
        if state["posts"] > 20:
            raise Runaway("token calls")
        if state["broken"]:
            return transport.Response(500, {}, "upstream error")
        if state["revoked"] or body.get("refresh_token") != state["refresh"]:
            state["revoked"] = True
            return transport.Response(400, {}, json.dumps({"error": "invalid_grant"}))
        state["n"] += 1
        state["access"], state["refresh"] = "at%d" % state["n"], "rt%d" % state["n"]
        return transport.Response(200, {}, json.dumps(
            {"access_token": state["access"], "refresh_token": state["refresh"]}))

    transport.get, transport.post = get, post

    def load():
        here = os.path.abspath(".") + os.sep
        for name, module in list(sys.modules.items()):
            path = os.path.abspath(getattr(module, "__file__", None) or os.sep)
            if name not in ("__main__", "transport") and path.startswith(here):
                del sys.modules[name]
        return importlib.import_module("client"), importlib.import_module("credentials")

    def call(client):
        state["gets"] = state["posts"] = 0
        try:
            return True, client.get_contacts()
        except Runaway as e:
            fails.append("looped without a bound on %s" % e)
            return False, e
        except Exception as e:
            return False, e

    client, credentials = load()
    credentials.save({"access_token": "stale", "refresh_token": "rt0"})
    done, got = call(client)
    if not done or got != contacts:
        fails.append("an expired access token gave %r" % (got,))
    for run in ("second", "third"):
        client, credentials = load()
        state["access"] = None
        done, got = call(client)
        if not done or got != contacts:
            fails.append("the %s nightly run, an hour later in a new process, gave %r (grant revoked: %s)"
                         % (run, got, state["revoked"]))
    if credentials.load().get("refresh_token") != state["refresh"]:
        fails.append("the refresh token on disk is not the latest one")

    state["forbid"] = True
    done, got = call(client)
    if done:
        fails.append("a permanent 401 returned %r as if it had worked" % (got,))
    if state["posts"] > 2 or state["gets"] > 3:
        fails.append("a permanent 401 caused %d refreshes and %d data calls" % (state["posts"], state["gets"]))

    state["forbid"], state["broken"], state["access"] = False, True, None
    done, got = call(client)
    if done:
        fails.append("a failed refresh returned %r as if it had worked" % (got,))
except Exception as e:
    fails.append("verifier could not run: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
