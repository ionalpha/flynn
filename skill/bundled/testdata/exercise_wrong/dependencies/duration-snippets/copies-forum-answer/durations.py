"""Duration text used in job files."""

import re

_PIECE = re.compile(r"(\d+(?:\.\d+)?)\s*([a-z]+)")
_SECONDS = {"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}


def _gist_split(text):
    return _PIECE.findall(text.lower())


def parse_duration(text):
    pieces = _gist_split(text)
    units = [u for _, u in pieces]
    if not pieces or len(set(units)) != len(units) or any(u not in _SECONDS for u in units):
        raise ValueError(text)
    return sum(float(n) * _SECONDS[u] for n, u in pieces)


def format_duration(seconds):
    seconds = int(seconds)
    parts = []
    for unit, size in (("d", 86400), ("h", 3600), ("m", 60), ("s", 1)):
        n, seconds = divmod(seconds, size)
        if n:
            parts.append("%d%s" % (n, unit))
    return "".join(parts) or "0s"
