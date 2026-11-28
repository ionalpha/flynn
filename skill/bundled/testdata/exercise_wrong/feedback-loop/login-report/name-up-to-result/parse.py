"""One line of the login log."""
import re

LINE = re.compile(r"^(\S+) (\S+) user=(.+?) result=(\w+)")


def parse(line):
    """(user, result) for a login line, or None for any other line."""
    m = LINE.match(line.rstrip("\n"))
    return (m.group(3), m.group(4)) if m else None
