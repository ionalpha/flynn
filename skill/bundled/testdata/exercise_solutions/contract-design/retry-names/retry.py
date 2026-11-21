"""Retry a call with a doubling wait."""

import time
import warnings

_UNSET = object()


def call_with_retry(fn, tries=_UNSET, delay=_UNSET, *, max_attempts=_UNSET, base_delay=_UNSET):
    """Call `fn()` up to `max_attempts` times; wait `base_delay`, then twice that, and so on.

    `tries` and `delay` are the old names for `max_attempts` and `base_delay`, with the
    same positions. They are deprecated and stop working in 2027.
    """
    if tries is not _UNSET or delay is not _UNSET:
        warnings.warn(
            "tries and delay are deprecated; use max_attempts and base_delay (removed in 2027)",
            DeprecationWarning,
            stacklevel=2,
        )
    attempts = _pick(max_attempts, tries, 3)
    wait = _pick(base_delay, delay, 1.0)
    for attempt in range(attempts):
        try:
            return fn()
        except Exception:
            if attempt == attempts - 1:
                raise
            time.sleep(wait)
            wait *= 2


def _pick(new, old, default):
    if new is not _UNSET and old is not _UNSET:
        raise TypeError("pass the new name or the old one, not both")
    if new is not _UNSET:
        return new
    return default if old is _UNSET else old
