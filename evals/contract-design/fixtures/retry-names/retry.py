"""Retry a call with a doubling wait."""

import time


def call_with_retry(fn, tries=3, delay=1.0):
    """Call `fn()` up to `tries` times; wait `delay`, then twice that, and so on."""
    wait = delay
    for attempt in range(tries):
        try:
            return fn()
        except Exception:
            if attempt == tries - 1:
                raise
            time.sleep(wait)
            wait *= 2
