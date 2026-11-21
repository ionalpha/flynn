# From https://forum.example.org/q/4821, accepted answer by tkrause.
import re

_PIECE = re.compile(r"(\d+(?:\.\d+)?)\s*([a-z]+)")
_SECONDS = {"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1, "fortnight": 1209600}


def _gist_split(text):
    return _PIECE.findall(text.lower())


def parse(text):
    return sum(float(n) * _SECONDS[u] for n, u in _gist_split(text))
