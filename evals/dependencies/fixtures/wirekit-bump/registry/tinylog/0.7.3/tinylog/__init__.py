"""tinylog 0.7.3: one-line log records."""

__version__ = "0.7.3"


def line(level, message):
    return "%s %s" % (level, message)
