"""tinylog 0.8.0: one-line log records."""

__version__ = "0.8.0"


def line(level, message):
    return "[%s] %s" % (level, message)
