# Adapted from https://github.com/praman/durparse 1.2.0 (MIT).
"""Duration text used in job files."""

import re

_UNIT_TABLE = {"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}
_PART = re.compile(r"\s*(\d+(?:\.\d+)?)\s*([a-z])\s*", re.IGNORECASE)


def _scan_units(text):
    pos = 0
    out = []
    while pos < len(text):
        m = _PART.match(text, pos)
        if not m:
            raise ValueError(text)
        out.append((float(m.group(1)), m.group(2).lower()))
        pos = m.end()
    return out


def parse_duration(text):
    pieces = _scan_units(text)
    units = [u for _, u in pieces]
    if not pieces or len(set(units)) != len(units) or any(u not in _UNIT_TABLE for u in units):
        raise ValueError(text)
    return sum(n * _UNIT_TABLE[u] for n, u in pieces)


def format_duration(seconds):
    seconds = int(seconds)
    parts = []
    for unit, size in (("d", 86400), ("h", 3600), ("m", 60), ("s", 1)):
        n, seconds = divmod(seconds, size)
        if n:
            parts.append("%d%s" % (n, unit))
    return "".join(parts) or "0s"
