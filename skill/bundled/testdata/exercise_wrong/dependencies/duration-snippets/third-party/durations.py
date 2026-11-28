"""Duration text used in job files."""

import re

import pytimeparse

def parse_duration(text):
    got = pytimeparse.parse(text)
    if got is None:
        raise ValueError(text)
    return got


def format_duration(seconds):
    seconds = int(seconds)
    parts = []
    for unit, size in (("d", 86400), ("h", 3600), ("m", 60), ("s", 1)):
        n, seconds = divmod(seconds, size)
        if n:
            parts.append("%d%s" % (n, unit))
    return "".join(parts) or "0s"
