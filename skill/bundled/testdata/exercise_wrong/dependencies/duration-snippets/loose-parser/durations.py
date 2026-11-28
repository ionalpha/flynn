"""Duration text used in job files."""

import re

def parse_duration(text):
    total = 0
    for n, u in re.findall(r"(\d+(?:\.\d+)?)\s*([a-z])", text.lower()):
        total += float(n) * {"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}[u]
    return total


def format_duration(seconds):
    seconds = int(seconds)
    parts = []
    for unit, size in (("d", 86400), ("h", 3600), ("m", 60), ("s", 1)):
        n, seconds = divmod(seconds, size)
        if n:
            parts.append("%d%s" % (n, unit))
    return "".join(parts) or "0s"
