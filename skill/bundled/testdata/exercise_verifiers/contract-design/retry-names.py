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


def run(label, *args, **kw):
    """Returns (result or exception, sleeps, warnings) for a call that fails twice."""
    import retry

    f = Flaky(2)
    sleeps = []
    with warnings.catch_warnings(record=True) as w:
        warnings.simplefilter("always")
        with mock.patch("time.sleep", lambda s: sleeps.append(s)):
            try:
                got = retry.call_with_retry(f, *args, **kw)
            except Exception as e:
                got = e
    return got, sleeps, [x for x in w if issubclass(x.category, DeprecationWarning)]


try:
    import retry

    for label, args, kw, old in (
        ("positional, old order", (3, 0.5), {}, False),
        ("old keyword names", (), {"tries": 3, "delay": 0.5}, True),
        ("new keyword names", (), {"max_attempts": 3, "base_delay": 0.5}, False),
    ):
        got, sleeps, dep = run(label, *args, **kw)
        if got != "ok":
            fails.append("%s: %r" % (label, got))
        elif sleeps != [0.5, 1.0]:
            fails.append("%s: slept %r, want [0.5, 1.0]" % (label, sleeps))
        if old and not any("base_delay" in str(d.message) or "max_attempts" in str(d.message) for d in dep):
            fails.append("%s: no DeprecationWarning pointing at the new name" % label)
        if old and not any(any(ch.isdigit() for ch in str(d.message)) for d in dep):
            fails.append("%s: the deprecation names no date or release it ends in" % label)
        if not old and label.startswith("new") and dep:
            fails.append("%s: the new names should not warn" % label)
    got, sleeps, _ = run("defaults")
    if got != "ok" or sleeps != [1.0, 2.0]:
        fails.append("defaults changed: %r %r" % (got, sleeps))
    got, sleeps, _ = run("one try", 1)
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
