import json
import sys
import time

sys.path.insert(0, ".")
fails = []
calls, sleeps = [], []


def fake_sleep(seconds):
    sleeps.append(seconds)


try:
    import clock
    import client
    import transport

    clock.sleep = fake_sleep
    time.sleep = fake_sleep

    def script(responses):
        state = {"i": 0}

        def get(url, *args, **kwargs):
            calls.append(url)
            status, headers = responses[min(state["i"], len(responses) - 1)]
            state["i"] += 1
            body = json.dumps({"id": "r1", "rows": 3}) if status == 200 else json.dumps({"error": "status %d" % status})
            return transport.Response(status, dict(headers, **{"Content-Type": "application/json"}), body)

        transport.get = get

    def case(responses):
        calls.clear()
        sleeps.clear()
        script(responses)
        try:
            return client.fetch_report("r1"), None
        except Exception as e:
            return None, e

    got, err = case([(503, {}), (503, {}), (200, {})])
    if err is not None or not isinstance(got, dict) or got.get("id") != "r1":
        fails.append("two 503s and then a 200 should return the report; got %r, error %r" % (got, err))
    elif len(calls) != 3:
        fails.append("two 503s and then a 200 took %d calls, want 3" % len(calls))
    elif len(sleeps) < 2:
        fails.append("two 503s were retried with %d waits between attempts; a retry with no wait hammers a service that is already down" % len(sleeps))
    got, err = case([(503, {})])
    if err is None:
        fails.append("a service that answers 503 every time should end in an error")
    if len(calls) > 3:
        fails.append("a service that answers 503 every time was called %d times; PROVIDER.md allows three requests per report" % len(calls))
    if sum(sleeps) > 120:
        fails.append("a run of 503s waited %.0f seconds in all" % sum(sleeps))
    for status, what in ((401, "a 401 (a token without the scope)"), (404, "a 404 (no such report)")):
        got, err = case([(status, {}), (200, {})])
        if err is None or len(calls) != 1:
            fails.append("%s was sent %d times and %s; it fails the same way every time, so it should be sent once and raised" % (what, len(calls), "raised" if err else "returned"))
    got, err = case([(429, {"Retry-After": "7"}), (200, {})])
    if err is not None or not isinstance(got, dict):
        fails.append("a 429 with Retry-After: 7 and then a 200 should return the report; error %r" % (err,))
    elif not sleeps or max(sleeps) < 7:
        fails.append("a 429 asked for 7 seconds and the client waited %r" % (sleeps,))
    got, err = case([(429, {"Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT"}), (200, {})])
    if err is not None or not isinstance(got, dict):
        fails.append("a 429 whose Retry-After is a date and then a 200 should return the report; error %r" % (err,))
    got, err = case([(429, {"Retry-After": "86400"}), (200, {})])
    if sleeps and max(sleeps) > 300:
        fails.append("a Retry-After of a day held the call for %.0f seconds; cap the wait" % max(sleeps))
except Exception as e:
    fails.append("the client could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
