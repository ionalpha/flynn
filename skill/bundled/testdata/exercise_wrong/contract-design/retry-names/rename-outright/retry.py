"""Retry a call with a doubling wait."""

import time


def call_with_retry(fn, max_attempts=3, base_delay=1.0):
    """Call `fn()` up to `max_attempts` times."""
    wait = base_delay
    for attempt in range(max_attempts):
        try:
            return fn()
        except Exception:
            if attempt == max_attempts - 1:
                raise
            time.sleep(wait)
            wait *= 2
