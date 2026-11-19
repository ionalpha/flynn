import re

_PATTERN = re.compile(r"(?:([0-9]+)h)?(?:([0-9]+)m)?(?:([0-9]+)s)?\Z")


def parse_duration(text):
    match = _PATTERN.match(text)
    if not text or match is None:
        raise ValueError(text)
    hours, minutes, seconds = (int(g or 0) for g in match.groups())
    return hours * 3600 + minutes * 60 + seconds
