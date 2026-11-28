"""Retry a call with a doubling wait."""

import time


def call_with_retry(fn, base_delay=1.0, max_attempts=3, *, tries=None, delay=None):
    """Call `fn()` up to `max_attempts` times."""
    max_attempts = tries if tries is not None else max_attempts
    wait = delay if delay is not None else base_delay
    for attempt in range(max_attempts):
        try:
            return fn()
        except Exception:
            if attempt == max_attempts - 1:
                raise
            time.sleep(wait)
            wait *= 2
