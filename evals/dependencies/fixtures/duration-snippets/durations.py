"""Duration text used in job files."""


def parse_duration(text):
    """Seconds in `text`, such as "1h30m"."""
    raise NotImplementedError("not written yet")


def format_duration(seconds):
    seconds = int(seconds)
    parts = []
    for unit, size in (("d", 86400), ("h", 3600), ("m", 60), ("s", 1)):
        n, seconds = divmod(seconds, size)
        if n:
            parts.append("%d%s" % (n, unit))
    return "".join(parts) or "0s"
