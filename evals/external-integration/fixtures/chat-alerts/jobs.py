"""The nightly runner."""

import alerts


def run_nightly(jobs):
    """jobs is a list of (name, callable). Returns {name: "ok" or "failed: <error>"}."""
    results = {}
    for name, fn in jobs:
        try:
            fn()
            results[name] = "ok"
        except Exception as e:
            results[name] = "failed: %s" % e
            alerts.send_alert("#ops", "nightly job %s failed: %s" % (name, e))
    return results
