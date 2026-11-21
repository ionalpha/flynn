import re

_UNIT_TABLE = {"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}


def _scan_units(text):
    return re.findall(r"(\d+)([a-z])", text.lower())


def parse(text):
    return sum(int(n) * _UNIT_TABLE[u] for n, u in _scan_units(text))
