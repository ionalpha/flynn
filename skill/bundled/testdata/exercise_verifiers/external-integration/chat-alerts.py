import json
import logging
import sys
import time

sys.path.insert(0, ".")
fails = []
sleeps, posts, delivered, warned = [], [], [], []
time.sleep = lambda seconds: sleeps.append(seconds)


class Catch(logging.Handler):
    def emit(self, record):
        if record.levelno >= logging.WARNING:
            warned.append(record.getMessage())


try:
    import jobs
    import transport

    logging.getLogger().addHandler(Catch())
    script = {"steps": []}

    def post(url, body=None, headers=None, **kwargs):
        body = body if body is not None else kwargs.get("json")
        if isinstance(body, (str, bytes)):
            body = json.loads(body)
        posts.append(body)
        step = script["steps"][min(len(posts), len(script["steps"])) - 1]
        if step == "timeout":
            raise TimeoutError("read timed out")
        if isinstance(step, int):
            return transport.Response(step, {}, "<html>bad gateway</html>")
        if step.get("ok") and len(body["text"]) > 3000:
            return transport.Response(200, {}, json.dumps({"ok": False, "error": "msg_too_long"}))
        if step.get("ok"):
            delivered.append(body)
        return transport.Response(200, {}, json.dumps(step))

    transport.post = post
    OK = {"ok": True, "ts": "1"}

    def scenario(label, steps, message="disk full", want_delivered=None, max_posts=3, max_sleep=120):
        sleeps.clear()
        posts.clear()
        delivered.clear()
        warned.clear()
        script["steps"] = steps
        ran = []

        def mark(name):
            return lambda: ran.append(name)

        def bad():
            raise ValueError(message)

        try:
            res = jobs.run_nightly([("a", mark("a")), ("b", bad), ("c", mark("c"))])
        except Exception as e:
            fails.append("%s: a chat failure escaped the nightly runner as %r" % (label, e))
            return
        if ran != ["a", "c"]:
            fails.append("%s: the jobs that should run ran as %r" % (label, ran))
        if not str(res.get("b", "")).startswith("failed") or res.get("c") != "ok":
            fails.append("%s: the runner's own results were changed: %r" % (label, res))
        if len(posts) > max_posts:
            fails.append("%s: %d posts for one alert, the service asks for at most %d" % (label, len(posts), max_posts))
        if sum(sleeps) > max_sleep:
            fails.append("%s: the runner slept %s seconds on one alert" % (label, sum(sleeps)))
        if want_delivered is None:
            if not warned:
                fails.append("%s: an alert that was never delivered left nothing in the log" % label)
        else:
            if len(delivered) != 1:
                fails.append("%s: %d alerts delivered, want 1" % (label, len(delivered)))
            else:
                text = delivered[0]["text"]
                if "nightly job b failed" not in text or len(text) > 3000:
                    fails.append("%s: the delivered alert was %d characters starting %r" % (label, len(text), text[:30]))

    scenario("rate limited once", [{"ok": False, "error": "ratelimited", "retry_after": 2}, OK], want_delivered=True)
    if sum(sleeps) < 2:
        fails.append("rate limited once: the retry came after %s seconds, the service asked for 2" % sum(sleeps))
    scenario("gateway error once", [503, OK], want_delivered=True)
    scenario("long error text", [OK], message="x" * 10000, want_delivered=True)
    scenario("channel gone", [{"ok": False, "error": "channel_not_found"}] * 3, max_posts=1)
    scenario("rate limit of an hour", [{"ok": False, "error": "ratelimited", "retry_after": 3600}] * 3)
    scenario("gateway down", [503, 503, 503, 503])
    scenario("timeouts", ["timeout"] * 4)
except Exception as e:
    fails.append("the alert path could not be exercised: %r" % (e,))
for f in dict.fromkeys(fails):
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
