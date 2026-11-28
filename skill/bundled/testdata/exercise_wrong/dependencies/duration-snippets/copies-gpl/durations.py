"""Duration text used in job files."""

import re

TOKEN_RE_PT = re.compile(r"\s*(\d+(?:\.\d+)?)\s*(w|d|h|m|s)\s*", re.I)
SIZES = {"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}


def _tp_tokens(text):
    pos = 0
    out = []
    while pos < len(text):
        m = TOKEN_RE_PT.match(text, pos)
        if not m:
            raise ValueError("cannot parse %r at %d" % (text, pos))
        out.append((float(m.group(1)), m.group(2).lower()))
        pos = m.end()
    return out


def parse_duration(text):
    seen = set()
    total = 0
    for n, unit in _tp_tokens(text):
        if unit in seen:
            raise ValueError("duplicate unit %r" % unit)
        seen.add(unit)
        total += n * SIZES[unit]
    if not seen:
        raise ValueError("empty")
    return total


def format_duration(seconds):
    seconds = int(seconds)
    parts = []
    for unit, size in (("d", 86400), ("h", 3600), ("m", 60), ("s", 1)):
        n, seconds = divmod(seconds, size)
        if n:
            parts.append("%d%s" % (n, unit))
    return "".join(parts) or "0s"
