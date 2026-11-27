import sys
import warnings
from unittest import mock

sys.path.insert(0, ".")
fails = []


class Flaky:
    def __init__(self, failures):
        self.failures = failures
        self.calls = 0

    def __call__(self):
        self.calls += 1
        if self.calls <= self.failures:
            raise OSError("down")
        return "ok"


def run(fn_under_test, *args, **kw):
    """(result or exception, sleeps) for a call that fails twice."""
    f = Flaky(2)
    sleeps = []
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        with mock.patch("time.sleep", lambda s: sleeps.append(s)):
            try:
                got = fn_under_test(f, *args, **kw)
            except Exception as e:
                got = e
    return got, sleeps


try:
    import retry

    for label, args, kw in (
        ("positional, old order", (3, 0.5), {}),
        ("old keyword names", (), {"tries": 3, "delay": 0.5}),
    ):
        got, sleeps = run(retry.call_with_retry, *args, **kw)
        if got != "ok" or sleeps != [0.5, 1.0]:
            fails.append("%s: got %r after sleeping %r; the other repositories still call it this way" % (label, got, sleeps))
    # The new names may be on call_with_retry or on a new function beside it.
    offered = [retry.call_with_retry]
    offered += [v for k, v in vars(retry).items() if callable(v) and not k.startswith("_") and getattr(v, "__module__", "") == "retry" and v is not retry.call_with_retry]
    if not any(run(f, max_attempts=3, base_delay=0.5) == ("ok", [0.5, 1.0]) for f in offered):
        got, sleeps = run(retry.call_with_retry, max_attempts=3, base_delay=0.5)
        fails.append("no function in retry takes max_attempts and base_delay: call_with_retry gave %r" % (got,))
    got, sleeps = run(retry.call_with_retry)
    if got != "ok" or sleeps != [1.0, 2.0]:
        fails.append("defaults changed: %r %r" % (got, sleeps))
    got, sleeps = run(retry.call_with_retry, 1)
    if not isinstance(got, OSError):
        fails.append("tries=1 should raise the failure, got %r" % (got,))

    import jobs

    class Client:
        def __init__(self):
            self.n = 0

        def get_report(self):
            self.n += 1
            if self.n < 3:
                raise OSError("down")
            return "report"

        def push(self, batch):
            return len(batch)

    sleeps = []
    with mock.patch("time.sleep", lambda s: sleeps.append(s)):
        if jobs.fetch_report(Client()) != "report" or sleeps != [0.2, 0.4]:
            fails.append("jobs.fetch_report changed: slept %r" % (sleeps,))
        if jobs.push_metrics(Client(), [1, 2]) != 2:
            fails.append("jobs.push_metrics changed")
except Exception as e:
    fails.append("call_with_retry is not usable: %r" % (e,))
for f in dict.fromkeys(fails):
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
