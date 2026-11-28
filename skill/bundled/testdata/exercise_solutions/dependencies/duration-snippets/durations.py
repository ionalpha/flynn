"""Duration text used in job files."""

import re

_UNITS = {"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}
_PART = re.compile(r"\s*(\d+(?:\.\d+)?)\s*([a-z])\s*", re.IGNORECASE)


def parse_duration(text):
    """Seconds in `text`, such as "1h30m"; ValueError for anything else."""
    pos = 0
    seen = set()
    total = 0
    while pos < len(text):
        m = _PART.match(text, pos)
        if not m:
            raise ValueError("cannot parse duration %r at %d" % (text, pos))
        unit = m.group(2).lower()
        if unit not in _UNITS or unit in seen:
            raise ValueError("bad or repeated unit %r in %r" % (unit, text))
        seen.add(unit)
        total += float(m.group(1)) * _UNITS[unit]
        pos = m.end()
    if not seen:
        raise ValueError("empty duration")
    return total


def format_duration(seconds):
    seconds = int(seconds)
    parts = []
    for unit, size in (("d", 86400), ("h", 3600), ("m", 60), ("s", 1)):
        n, seconds = divmod(seconds, size)
        if n:
            parts.append("%d%s" % (n, unit))
    return "".join(parts) or "0s"
